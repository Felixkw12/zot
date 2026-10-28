package mcp

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
)

// ErrSignInRequired marks a server that needs an OAuth sign-in.
var ErrSignInRequired = errors.New("sign-in required")

// tokenFile stores OAuth credentials for all servers, keyed by server name
// and URL so a token is never sent to a different endpoint.
type tokenFile struct {
	Servers map[string]storedCredentials `json:"servers"`
}

type storedCredentials struct {
	URL          string `json:"url"`
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"`
	// RedirectURI is the URI the stored client was registered with. A
	// client can only be reused on the same callback port.
	RedirectURI string           `json:"redirectUri,omitempty"`
	Token       *transport.Token `json:"token,omitempty"`
	// ResourceMetadataURL retains challenge discovery for token refresh.
	// Its origin is checked again before it is passed to the transport.
	ResourceMetadataURL string `json:"resourceMetadataUrl,omitempty"`
}

// tokenStore keeps OAuth credentials in one private file. Access is
// serialized within the process.
type tokenStore struct {
	path string
	mu   sync.Mutex
	// refreshLocks serialize complete refreshes per credential, not just
	// individual file reads and writes. They are shared across connections.
	refreshLocks map[string]chan struct{}
	// generations invalidate connections and pending sign-ins on logout.
	// Checked under mu with each credential read or write.
	generations map[string]uint64
}

func newTokenStore(home string) *tokenStore {
	return &tokenStore{path: filepath.Join(home, "mcp-auth.json")}
}

func (s *tokenStore) read() (tokenFile, error) {
	file := tokenFile{Servers: map[string]storedCredentials{}}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return file, nil
	}
	if err != nil {
		return file, err
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return file, fmt.Errorf("%s is not valid JSON", s.path)
	}
	if file.Servers == nil {
		file.Servers = map[string]storedCredentials{}
	}
	return file, nil
}

// snapshot reads the whole file once, for callers that check several
// servers without repeating the file read for each.
func (s *tokenStore) snapshot() tokenFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	file, _ := s.read()
	return file
}

func (s *tokenStore) load(key string) (storedCredentials, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	file, err := s.read()
	if err != nil {
		return storedCredentials{}, false, err
	}
	entry, ok := file.Servers[key]
	return entry, ok, nil
}

func (s *tokenStore) update(key string, change func(*storedCredentials, bool)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateLocked(key, change)
}

func (s *tokenStore) updateLocked(key string, change func(*storedCredentials, bool)) error {
	file, err := s.read()
	if err != nil {
		return err
	}
	entry, ok := file.Servers[key]
	change(&entry, ok)
	if entry == (storedCredentials{}) {
		delete(file.Servers, key)
	} else {
		file.Servers[key] = entry
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	// writeFileAtomic keeps an existing file's mode, so force it private.
	if err := writeFileAtomic(s.path, append(data, '\n')); err != nil {
		return err
	}
	return os.Chmod(s.path, 0o600)
}

func credentialKey(name, url string) string {
	digest := sha256.Sum256([]byte(url))
	return name + "@" + hex.EncodeToString(digest[:6])
}

func (s *tokenStore) refreshLock(key string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refreshLocks == nil {
		s.refreshLocks = map[string]chan struct{}{}
	}
	if s.refreshLocks[key] == nil {
		s.refreshLocks[key] = make(chan struct{}, 1)
	}
	return s.refreshLocks[key]
}

// serverTokens adapts the store to the client library for one server.
// Refresh here so the SDK never reads an expired token and independently
// replays its refresh token on concurrent tool, resource, or listener calls.
type serverTokens struct {
	store      *tokenStore
	key        string
	url        string
	refresh    func(context.Context, string) (*transport.Token, error)
	generation uint64
}

// update rejects saves from a connection or sign-in that predates logout.
// It must not acquire the refresh gate: refresh saves already hold it.
func (t *serverTokens) update(change func(*storedCredentials, bool)) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	if t.generation != t.store.generations[t.key] {
		return transport.ErrOAuthAuthorizationRequired
	}
	return t.store.updateLocked(t.key, change)
}

func (t *serverTokens) load() (storedCredentials, bool, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	if t.generation != t.store.generations[t.key] {
		return storedCredentials{}, false, transport.ErrOAuthAuthorizationRequired
	}
	file, err := t.store.read()
	entry, ok := file.Servers[t.key]
	return entry, ok, err
}

func (t *serverTokens) GetToken(ctx context.Context) (*transport.Token, error) {
	gate := t.store.refreshLock(t.key)
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entry, ok, err := t.load()
	if err != nil {
		return nil, err
	}
	if !ok || entry.Token == nil || entry.URL != t.url {
		return nil, transport.ErrNoToken
	}
	// Re-read after acquiring the gate so waiters use a token that an
	// earlier caller has already refreshed and saved.
	if !entry.Token.IsExpired() && entry.Token.AccessToken != "" {
		return entry.Token, nil
	}
	if entry.Token.RefreshToken != "" && t.refresh != nil {
		token, err := t.refresh(ctx, entry.Token.RefreshToken)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err == nil && token != nil && !token.IsExpired() && token.AccessToken != "" {
			return token, nil
		}
	}
	return nil, transport.ErrOAuthAuthorizationRequired
}

func (t *serverTokens) SaveToken(ctx context.Context, token *transport.Token) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return t.update(func(entry *storedCredentials, _ bool) {
		entry.URL, entry.Token = t.url, token
	})
}

// oauthEnabled reports whether a server authenticates with OAuth: HTTP
// servers without a static Authorization header.
func oauthEnabled(cfg ServerConfig) bool {
	return cfg.Transport != "stdio" && !cfg.hasAuthorizationHeader()
}

func requiresSignIn(cfg ServerConfig, err error) bool {
	return oauthEnabled(cfg) && (client.IsAuthorizationRequiredError(err) ||
		client.IsOAuthAuthorizationRequiredError(err) || errors.Is(err, transport.ErrOAuthAuthorizationRequired))
}

// markSignInRequired withdraws capabilities after runtime authorization fails.
// A late failure from a replaced connection must not affect its replacement.
// notify is false for pre-turn polling, whose caller refreshes the registry.
func (m *Manager) markSignInRequired(s *server, err error, notify bool) bool {
	if !requiresSignIn(s.cfg, err) {
		return false
	}
	m.mu.Lock()
	if m.servers[s.name] != s || s.closing.Load() || errors.Is(s.err, ErrSignInRequired) {
		m.mu.Unlock()
		return false
	}
	s.err = ErrSignInRequired
	if metadata := sameOriginMetadataURL(s.cfg.URL, authorizationMetadataURL(err)); metadata != "" {
		s.authMetadataURL = metadata
	}
	m.rebuildLocked()
	callback := m.onChange
	m.mu.Unlock()
	if notify && callback != nil {
		callback()
	}
	return true
}

// redirectURI is fixed per configured port so a registered client keeps
// working. Without a port the sign-in picks a free one and registers the
// client for it.
func redirectURI(port int) string {
	return "http://127.0.0.1:" + strconv.Itoa(port) + "/callback"
}

// oauthTransportConfig builds the library configuration for a server. The
// client ID comes from the configuration or from an earlier registration.
func (m *Manager) oauthTransportConfig(name string, cfg ServerConfig, redirect string) (transport.OAuthConfig, error) {
	key := credentialKey(name, cfg.URL)
	m.tokens.mu.Lock()
	tokens := &serverTokens{store: m.tokens, key: key, url: cfg.URL, generation: m.tokens.generations[key]}
	m.tokens.mu.Unlock()
	out := transport.OAuthConfig{
		RedirectURI: redirect,
		TokenStore:  tokens,
		PKCEEnabled: true,
	}
	if cfg.OAuth != nil {
		out.ClientID, out.ClientSecret = cfg.OAuth.ClientID, cfg.OAuth.ClientSecret
		out.Scopes = cfg.OAuth.Scopes
		out.AuthServerMetadataURL = cfg.OAuth.AuthServerMetadataURL
	}
	entry, ok, err := tokens.load()
	if err != nil {
		return out, err
	}
	if ok && entry.URL == cfg.URL {
		if out.ClientID == "" {
			out.ClientID, out.ClientSecret = entry.ClientID, entry.ClientSecret
		}
		out.ProtectedResourceMetadataURL = sameOriginMetadataURL(cfg.URL, entry.ResourceMetadataURL)
	}
	if out.RedirectURI == "" {
		// Refreshes do not redirect, but the library requires a value. Use
		// the URI the stored client was registered with, so a strict
		// authorization server sees the same client parameters.
		switch {
		case cfg.OAuth != nil && cfg.OAuth.CallbackPort > 0:
			out.RedirectURI = redirectURI(cfg.OAuth.CallbackPort)
		case ok && entry.URL == cfg.URL && entry.RedirectURI != "":
			out.RedirectURI = entry.RedirectURI
		default:
			out.RedirectURI = redirectURI(1)
		}
	}
	// RefreshToken saves through this adapter but does not call GetToken,
	// so the gate remains held through the entire exchange and save.
	handler := transport.NewOAuthHandler(out)
	handler.SetBaseURL(cfg.URL)
	tokens.refresh = handler.RefreshToken
	return out, nil
}

// sameOriginMetadataURL applies the SDK's challenge-origin restriction before
// using a persisted URL as trusted OAuth configuration. Discovery still checks
// that the returned metadata identifies the requested protected resource.
func sameOriginMetadataURL(resource, metadata string) string {
	base, err := neturl.Parse(resource)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return ""
	}
	candidate, err := neturl.Parse(metadata)
	if err != nil || !strings.EqualFold(candidate.Scheme, base.Scheme) || !strings.EqualFold(candidate.Host, base.Host) {
		return ""
	}
	return metadata
}

// authorizationMetadataURL retains challenge metadata across the connection
// failure and the later interactive sign-in, including OAuth-wrapped errors.
func authorizationMetadataURL(err error) string {
	var oauth *transport.OAuthAuthorizationRequiredError
	if errors.As(err, &oauth) {
		return oauth.ResourceMetadataURL
	}
	var required *transport.AuthorizationRequiredError
	if errors.As(err, &required) {
		return required.ResourceMetadataURL
	}
	return ""
}

// SignInStatus reports progress of an interactive sign-in.
type SignInStatus struct {
	URL string // authorization URL, set once the browser is opened
	// Paste completes the sign-in with the URL the browser was redirected
	// to, for a browser on another machine where the local callback is not
	// reachable. It accepts the full redirect URL, a bare code, or
	// code#state, and reports an invalid value without ending the sign-in.
	Paste func(string) error
	// Done is closed when the sign-in ends, so a goroutine reading the
	// pasted address can stop.
	Done <-chan struct{}
}

// SignIn runs the OAuth authorization code flow with PKCE for a server and
// stores the resulting token. It opens the browser, waits for the redirect
// on a local callback, and returns when the token is saved or ctx ends.
// progress receives the authorization URL so the UI can show it.
func (m *Manager) SignIn(ctx context.Context, name string, progress func(SignInStatus)) error {
	m.mu.RLock()
	s := m.servers[name]
	metadataURL := ""
	if s != nil {
		metadataURL = s.authMetadataURL
	}
	m.mu.RUnlock()
	if s == nil {
		return fmt.Errorf("server %q is not configured", name)
	}
	cfg := s.cfg
	if !oauthEnabled(cfg) {
		return fmt.Errorf("server %q does not use OAuth", name)
	}
	key := credentialKey(name, cfg.URL)
	configuredClient := cfg.OAuth != nil && cfg.OAuth.ClientID != ""
	port := 0
	if cfg.OAuth != nil {
		port = cfg.OAuth.CallbackPort
	}
	// Reuse the port of an earlier registration so its client stays valid
	// and sign-in does not register a new client each time.
	stored, _, err := m.tokens.load(key)
	if err != nil {
		return err
	}
	listener, err := listenCallback(port, stored, cfg.URL, configuredClient)
	if err != nil {
		return fmt.Errorf("cannot listen for the sign-in callback: %w", err)
	}
	redirect := redirectURI(listener.Addr().(*net.TCPAddr).Port)
	options, err := m.oauthTransportConfig(name, cfg, redirect)
	if err != nil {
		listener.Close()
		return err
	}
	// A registered client is bound to its redirect URI.
	if !configuredClient && stored.RedirectURI != redirect {
		options.ClientID, options.ClientSecret = "", ""
	}
	if metadata := sameOriginMetadataURL(cfg.URL, metadataURL); metadata != "" {
		options.ProtectedResourceMetadataURL = metadata
	}
	handler := transport.NewOAuthHandler(options)
	handler.SetBaseURL(cfg.URL)
	if options.ClientID == "" {
		if err := handler.RegisterClient(ctx, "zot"); err != nil {
			listener.Close()
			return fmt.Errorf("client registration failed, configure oauth.clientId: %w", describeOAuthError(err))
		}
		if err := options.TokenStore.(*serverTokens).update(func(entry *storedCredentials, _ bool) {
			entry.URL, entry.ClientID, entry.ClientSecret, entry.RedirectURI = cfg.URL, handler.GetClientID(), handler.GetClientSecret(), redirect
		}); err != nil {
			listener.Close()
			return err
		}
	}
	verifier, err := transport.GenerateCodeVerifier()
	if err != nil {
		listener.Close()
		return err
	}
	state, err := transport.GenerateState()
	if err != nil {
		listener.Close()
		return err
	}
	authURL, err := handler.GetAuthorizationURL(ctx, state, transport.GenerateCodeChallenge(verifier))
	if err != nil {
		listener.Close()
		return describeOAuthError(err)
	}
	type callback struct{ code, err string }
	results := make(chan callback, 1)
	deliver := func(result callback) {
		select {
		case results <- result:
		default:
		}
	}
	server := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		query := r.URL.Query()
		// Requests without this sign-in's state come from elsewhere, for
		// example another local process or an old browser tab. They are
		// refused without ending the sign-in, which keeps waiting.
		if subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(state)) != 1 {
			http.Error(w, "This sign-in link is not valid anymore. Start the sign-in again in zot.", http.StatusBadRequest)
			return
		}
		result := callback{code: query.Get("code"), err: query.Get("error")}
		if description := query.Get("error_description"); description != "" {
			result.err += ": " + description
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if result.err != "" || result.code == "" {
			fmt.Fprintf(w, "<p>Sign-in failed: %s</p><p>You can close this window.</p>", html.EscapeString(result.err))
		} else {
			// The token exchange has not run yet, so do not claim success.
			// zot reports the outcome.
			fmt.Fprint(w, "<p>Authorization received. You can close this window and return to zot, which finishes the sign-in.</p>")
		}
		deliver(result)
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	paste := func(input string) error {
		code, pastedState, problem := parsePastedRedirect(input)
		if problem != "" {
			return errors.New(problem)
		}
		// A bare code carries no state. It is accepted only because the
		// user copied it from this sign-in, the PKCE verifier still binds
		// it to this request.
		if pastedState != "" && subtle.ConstantTimeCompare([]byte(pastedState), []byte(state)) != 1 {
			return errors.New("this URL belongs to another sign-in, copy the address from the latest browser tab")
		}
		deliver(callback{code: code})
		return nil
	}
	done := make(chan struct{})
	defer close(done)
	if progress != nil {
		progress(SignInStatus{URL: authURL, Paste: paste, Done: done})
	}
	openBrowser(authURL)
	var result callback
	select {
	case <-ctx.Done():
		return ctx.Err()
	case result = <-results:
	}
	if result.err != "" {
		return fmt.Errorf("authorization denied: %s", result.err)
	}
	if result.code == "" {
		return errors.New("authorization returned no code")
	}
	if err := handler.ProcessAuthorizationResponse(ctx, result.code, state, verifier); err != nil {
		return fmt.Errorf("token exchange failed: %w", describeOAuthError(err))
	}
	return options.TokenStore.(*serverTokens).update(func(entry *storedCredentials, _ bool) {
		entry.ResourceMetadataURL = options.ProtectedResourceMetadataURL
	})
}

// describeOAuthError keeps the structured OAuth error code and description
// an authorization server returned, and otherwise replaces the library's
// message, which can quote a raw response body or URL, with a short hint.
func describeOAuthError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var oauthErr transport.OAuthError
	if errors.As(err, &oauthErr) {
		return fmt.Errorf("authorization server answered %s", strings.TrimPrefix(oauthErr.Error(), "OAuth error: "))
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "no such host") || strings.Contains(text, "connection refused") || strings.Contains(text, "timeout") || strings.Contains(text, "deadline"):
		return errors.New("the authorization server could not be reached")
	case strings.Contains(text, "metadata"):
		return errors.New("authorization server metadata could not be discovered, configure oauth.authServerMetadataUrl")
	case strings.Contains(text, "registration"):
		return errors.New("the authorization server refused the client registration")
	case strings.Contains(text, "redirect uri"):
		return errors.New("the redirect URI is invalid, check oauth.callbackPort")
	}
	return errors.New("the authorization server rejected the request")
}

// parsePastedRedirect reads what the user copied from the browser: the full
// redirect URL, a bare authorization code, or code#state. An error query
// parameter from the authorization server is reported as such.
func parsePastedRedirect(input string) (code, state, problem string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", "", "paste the address the browser shows after signing in"
	}
	if strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://") {
		parsed, err := neturl.Parse(input)
		if err != nil {
			return "", "", "that is not a valid URL"
		}
		query := parsed.Query()
		if denied := query.Get("error"); denied != "" {
			return "", "", "the authorization server refused the sign-in: " + denied
		}
		if query.Get("code") == "" {
			return "", "", "the URL has no code, copy the address after the sign-in finished"
		}
		return query.Get("code"), query.Get("state"), ""
	}
	if code, state, ok := strings.Cut(input, "#"); ok {
		return code, state, ""
	}
	if strings.ContainsAny(input, " \t") {
		return "", "", "paste the full address from the browser"
	}
	return input, "", ""
}

// listenCallback opens the local callback listener. A configured port is
// required. Otherwise the port of a stored registration is reused when it
// is free, so the registered client keeps working, and any free port is
// used as a fallback.
func listenCallback(port int, stored storedCredentials, url string, configuredClient bool) (net.Listener, error) {
	if port != 0 {
		return net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	}
	if !configuredClient && stored.URL == url && stored.RedirectURI != "" {
		if parsed, err := neturl.Parse(stored.RedirectURI); err == nil {
			if listener, err := net.Listen("tcp", parsed.Host); err == nil {
				return listener, nil
			}
		}
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

// SignOut deletes stored credentials for a server, including a registered
// client, so the next sign-in starts fresh.
func (m *Manager) SignOut(name string) error {
	m.mu.RLock()
	s := m.servers[name]
	m.mu.RUnlock()
	if s == nil {
		return fmt.Errorf("server %q is not configured", name)
	}
	key := credentialKey(name, s.cfg.URL)
	m.tokens.mu.Lock()
	defer m.tokens.mu.Unlock()
	if m.tokens.generations == nil {
		m.tokens.generations = map[string]uint64{}
	}
	// Invalidate first, even if deleting the file entry fails. Pending
	// network operations must not restore credentials after logout.
	m.tokens.generations[key]++
	return m.tokens.updateLocked(key, func(entry *storedCredentials, _ bool) {
		*entry = storedCredentials{}
	})
}

// signedIn reports whether a token is stored for the server.
func (m *Manager) signedIn(name string, cfg ServerConfig) bool {
	entry, ok, err := m.tokens.load(credentialKey(name, cfg.URL))
	return err == nil && ok && entry.Token != nil && entry.URL == cfg.URL
}

// openBrowser is replaced in tests.
var openBrowser = func(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if cmd.Start() == nil {
		go func() { _ = cmd.Wait() }()
	}
}
