package mcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	protocol "github.com/mark3labs/mcp-go/mcp"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// Manager owns connections and the tool catalog for one runtime. Each
// server connects independently, and the catalog is rebuilt whenever a
// server's tools change, so withdrawn tools cannot remain callable.
type Manager struct {
	home, cwd string
	lifecycle sync.Mutex
	mu        sync.RWMutex
	servers   map[string]*server
	tools     map[string]core.Tool
	// invalid lists rejected entries so the manager can still show them.
	invalid        map[string]string
	invalidSources map[string]string
	fileErrs       []string
	loaded         bool
	// loadedCWD is the directory the current server list was read for.
	loadedCWD string
	tokens    *tokenStore
	// onChange runs, without locks held, after the catalog changed outside
	// a Reload, for example when a server announced new tools.
	onChange func()
	// generation invalidates refreshes started before a Reload or Close.
	generation uint64
}

type server struct {
	name     string
	cfg      ServerConfig
	raw      ServerConfig
	source   string
	override bool
	client   *client.Client
	process  *exec.Cmd
	cancel   context.CancelFunc
	err      error
	// lost is set when an established connection drops, for example
	// when a stdio server exits. Calls and the manager read it.
	lost    atomic.Bool
	closing atomic.Bool
	count   int
	tools   []ToolInfo
	defs    []toolDefinition
	// reconnecting serializes on-demand reconnects of this server.
	reconnecting sync.Mutex
	// http is the streamable HTTP transport, used to learn whether the
	// server keeps an event stream open for change notifications.
	http *retryTransport
	// listed is when the tool list was last read, in UnixNano.
	listed atomic.Int64
	// relisting prevents overlapping reads of the same server's list.
	relisting atomic.Bool
	// resources reports that the server offers MCP resources.
	resources bool
}

// relistInterval is how old a tool list may be before a server without
// change notifications is asked again before a turn. Tests shorten it.
var relistInterval atomic.Int64

func init() { relistInterval.Store(int64(30 * time.Second)) }

// pushesChanges reports whether the server can announce tool-list changes
// itself: stdio and SSE always deliver notifications, streamable HTTP only
// while it keeps an event stream open.
func (s *server) pushesChanges() bool {
	if s.cfg.Transport != "streamable-http" {
		return true
	}
	return s.http != nil && s.http.streaming()
}

func New(home, cwd string) *Manager {
	return &Manager{home: home, cwd: cwd, servers: map[string]*server{}, tools: map[string]core.Tool{}, tokens: newTokenStore(home)}
}

// OnChange registers a callback for catalog changes that happen outside
// Reload, such as a server announcing a changed tool list or a server
// reconnecting on its own. The host uses it to refresh the agent's tools.
func (m *Manager) OnChange(fn func()) {
	m.mu.Lock()
	m.onChange = fn
	m.mu.Unlock()
}

// Keep tool IDs within the common provider limit while retaining stable names.
func toolName(server, tool string) string {
	name := "mcp__" + invalidName.ReplaceAllString(server, "_") + "__" + invalidName.ReplaceAllString(tool, "_")
	if len(name) > 64 {
		digest := sha256.Sum256([]byte(name))
		name = fmt.Sprintf("%s_%x", name[:47], digest[:8])
	}
	return name
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Reload connects enabled servers in parallel. Invalid or failed servers are
// reported independently and do not prevent healthy servers from loading.
func (m *Manager) Reload(ctx context.Context) []error {
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	m.close()
	m.mu.RLock()
	cwd := m.cwd
	m.mu.RUnlock()
	loaded := loadConfigWithSources(m.home, cwd)
	errs := loaded.errs
	servers := map[string]*server{}
	for name, cfg := range loaded.configs {
		servers[name] = &server{name: name, cfg: cfg, raw: loaded.raw[name], source: loaded.sources[name], override: loaded.overrides[name]}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, s := range servers {
		if s.disabled() {
			continue
		}
		wg.Add(1)
		go func(s *server) {
			defer wg.Done()
			if err := m.start(ctx, s); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(s)
	}
	wg.Wait()
	m.mu.Lock()
	m.servers, m.invalid, m.invalidSources = servers, loaded.invalid, loaded.sources
	m.fileErrs = loaded.fileErrs
	m.loaded = true
	m.loadedCWD = cwd
	errs = append(errs, m.rebuildLocked()...)
	m.mu.Unlock()
	return errs
}

func (s *server) disabled() bool { return s.cfg.Enabled != nil && !*s.cfg.Enabled }

// start connects one server and records its tools or error. Errors never
// contain transport details, which can include URLs, headers, or arguments.
func (m *Manager) start(ctx context.Context, s *server) error {
	// Open servers connect without OAuth. OAuth is used when a token or an
	// explicit oauth block exists, or after the server answers 401.
	useOAuth := oauthEnabled(s.cfg) && (s.cfg.OAuth != nil || m.signedIn(s.name, s.cfg))
	c, definitions, err := m.connect(ctx, s, useOAuth)
	if err != nil && !useOAuth && oauthEnabled(s.cfg) && client.IsAuthorizationRequiredError(err) {
		err = fmt.Errorf("%w: server requires authorization", ErrSignInRequired)
	}
	s.client, s.err, s.defs = c, err, nil
	s.tools, s.count = nil, 0
	s.lost.Store(false)
	if errors.Is(err, ErrSignInRequired) {
		return fmt.Errorf("MCP server %q: sign-in required, open /mcp and press l", s.name)
	}
	if err != nil {
		return fmt.Errorf("MCP server %q: connection or tool discovery failed", s.name)
	}
	if err := checkNames(s.name, definitions); err != nil {
		s.err = err
		s.shutdown()
		s.client = nil
		return fmt.Errorf("MCP server %q: %v", s.name, err)
	}
	s.setTools(definitions)
	return nil
}

func (s *server) setTools(definitions []toolDefinition) {
	s.listed.Store(time.Now().UnixNano())
	s.defs = definitions
	s.count = len(definitions)
	s.tools = s.tools[:0]
	for _, definition := range definitions {
		s.tools = append(s.tools, ToolInfo{Name: definition.Name, Description: firstLine(definition.Description), Exposure: s.cfg.exposureFor(definition.Name)})
	}
}

// checkNames rejects servers whose tool names collide after normalization,
// since calls could otherwise reach the wrong tool.
func checkNames(name string, definitions []toolDefinition) error {
	seen := map[string]bool{}
	for _, definition := range definitions {
		catalogName := toolName(name, definition.Name)
		if seen[catalogName] {
			return errors.New("tool names collide after normalization")
		}
		seen[catalogName] = true
	}
	return nil
}

// rebuildLocked derives the tool catalog from every connected server.
// Tool names that collide across servers are disabled.
func (m *Manager) rebuildLocked() []error {
	var errs []error
	tools := map[string]core.Tool{}
	collisions := map[string]bool{}
	for _, name := range sortedKeys(m.servers) {
		s := m.servers[name]
		if s.client == nil || s.err != nil {
			continue
		}
		for _, definition := range s.defs {
			exposure := s.cfg.exposureFor(definition.Name)
			if exposure == "hidden" {
				continue
			}
			nameInCatalog := toolName(name, definition.Name)
			if _, exists := tools[nameInCatalog]; exists || collisions[nameInCatalog] {
				delete(tools, nameInCatalog)
				collisions[nameInCatalog] = true
				errs = append(errs, fmt.Errorf("MCP tool %q: colliding name, tool disabled", nameInCatalog))
				continue
			}
			tools[nameInCatalog] = &remoteTool{name: nameInCatalog, description: definition.Description, schema: toolSchema(definition.InputSchema), remoteName: definition.Name, namespace: name, server: s, deferred: exposure == "deferred", manager: m}
		}
	}
	m.addResourceTools(tools)
	if len(tools) > 0 {
		tools["mcp__search_tools"] = &searchTool{manager: m}
	}
	m.tools = tools
	return errs
}

// Reconnect restarts one server and leaves the others connected. It reads
// the server's current configuration from the files first.
func (m *Manager) Reconnect(ctx context.Context, name string) error {
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	m.mu.RLock()
	old := m.servers[name]
	cwd := m.cwd
	m.mu.RUnlock()
	loaded := loadConfigWithSources(m.home, cwd)
	cfg, ok := loaded.configs[name]
	if !ok || old == nil {
		return fmt.Errorf("server %q is not configured, reload MCP after editing the file", name)
	}
	fresh := &server{name: name, cfg: cfg, raw: loaded.raw[name], source: loaded.sources[name], override: loaded.overrides[name]}
	if old.client != nil {
		old.closing.Store(true)
		old.shutdown()
	}
	var err error
	if !fresh.disabled() {
		err = m.start(ctx, fresh)
	}
	m.mu.Lock()
	m.servers[name] = fresh
	m.generation++
	m.rebuildLocked()
	m.mu.Unlock()
	return err
}

// relist reads a server's tools again after it announced a change, then
// rebuilds the catalog and notifies the host. New tools become callable and
// withdrawn ones unreachable. A failed relist keeps the previous list. It
// reports whether the catalog changed.
func (m *Manager) relist(s *server) bool {
	return m.relistContext(context.Background(), s, true)
}

func (m *Manager) relistContext(ctx context.Context, s *server, notify bool) bool {
	if !s.relisting.CompareAndSwap(false, true) {
		return false
	}
	defer s.relisting.Store(false)
	m.mu.RLock()
	generation := m.generation
	current := m.servers[s.name] == s
	m.mu.RUnlock()
	if !current || s.closing.Load() || s.client == nil || s.lost.Load() {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.ConnectTimeout)*time.Second)
	defer cancel()
	definitions, err := listTools(ctx, s.client)
	if err != nil || checkNames(s.name, definitions) != nil {
		return false
	}
	m.mu.Lock()
	if m.generation != generation || m.servers[s.name] != s {
		m.mu.Unlock()
		return false
	}
	changed := !sameDefinitions(s.defs, definitions)
	s.setTools(definitions)
	if changed {
		m.rebuildLocked()
	}
	callback := m.onChange
	m.mu.Unlock()
	if changed && notify && callback != nil {
		callback()
	}
	return changed
}

// sameDefinitions compares tool lists, so an unchanged list does not
// rebuild the catalog or refresh the agent.
func sameDefinitions(a, b []toolDefinition) bool {
	if len(a) != len(b) {
		return false
	}
	for idx := range a {
		if a[idx].Name != b[idx].Name || a[idx].Description != b[idx].Description || !bytes.Equal(a[idx].InputSchema, b[idx].InputSchema) {
			return false
		}
	}
	return true
}

// RefreshStale reads the tool lists of servers that cannot announce
// changes, when the last read is older than the refresh interval. Hosts
// call it before a prompt, so tools changed on such servers apply on the
// next turn. Servers are read in parallel and failures keep the previous
// list. It reports whether any catalog changed. OnChange is not called,
// the caller refreshes its registry itself when the result is true.
func (m *Manager) RefreshStale(ctx context.Context) bool {
	m.mu.RLock()
	var due []*server
	for _, s := range m.servers {
		if s.client == nil || s.err != nil || s.lost.Load() || s.pushesChanges() {
			continue
		}
		if time.Since(time.Unix(0, s.listed.Load())) >= time.Duration(relistInterval.Load()) {
			due = append(due, s)
		}
	}
	m.mu.RUnlock()
	var wg sync.WaitGroup
	var changed atomic.Bool
	for _, s := range due {
		wg.Add(1)
		go func(s *server) {
			defer wg.Done()
			if m.relistContext(ctx, s, false) {
				changed.Store(true)
			}
		}(s)
	}
	wg.Wait()
	return changed.Load()
}

// revive reconnects a dropped server before a call. Only one caller
// reconnects at a time, others wait and reuse the result.
func (m *Manager) revive(ctx context.Context, s *server) (*server, error) {
	s.reconnecting.Lock()
	defer s.reconnecting.Unlock()
	m.mu.RLock()
	current := m.servers[s.name]
	m.mu.RUnlock()
	if current != s {
		// Another caller already replaced the connection.
		if current == nil || current.err != nil || current.lost.Load() {
			return nil, fmt.Errorf("MCP server %q is disconnected, run /mcp reload to reconnect", s.name)
		}
		return current, nil
	}
	if !s.lost.Load() {
		return s, nil
	}
	if !m.lifecycle.TryLock() {
		return nil, fmt.Errorf("MCP server %q is reconnecting, try again", s.name)
	}
	defer m.lifecycle.Unlock()
	fresh := &server{name: s.name, cfg: s.cfg, raw: s.raw, source: s.source, override: s.override}
	s.closing.Store(true)
	s.shutdown()
	err := m.start(ctx, fresh)
	m.mu.Lock()
	if m.servers[s.name] != s {
		m.mu.Unlock()
		fresh.closing.Store(true)
		fresh.shutdown()
		return nil, fmt.Errorf("MCP server %q was reloaded, try again", s.name)
	}
	m.servers[s.name] = fresh
	m.generation++
	m.rebuildLocked()
	callback := m.onChange
	m.mu.Unlock()
	if callback != nil {
		callback()
	}
	if err != nil {
		if errors.Is(fresh.err, ErrSignInRequired) {
			return nil, fmt.Errorf("MCP server %q needs sign-in, open /mcp and press l", s.name)
		}
		return nil, fmt.Errorf("MCP server %q is disconnected and reconnecting failed, check /mcp", s.name)
	}
	return fresh, nil
}

// shutdown releases a server's connection and process.
func (s *server) shutdown() {
	if s.cancel != nil {
		s.cancel()
	}
	terminateProcess(s.process)
	if s.client != nil {
		s.client.Close()
	}
}

func (m *Manager) connect(ctx context.Context, s *server, useOAuth bool) (*client.Client, []toolDefinition, error) {
	cfg := s.cfg
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.ConnectTimeout)*time.Second)
	defer cancel()
	var c *client.Client
	var err error
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	switch cfg.Transport {
	case "stdio":
		env := make([]string, 0, len(cfg.Env))
		for k, v := range cfg.Env {
			env = append(env, k+"="+v)
		}
		c, err = client.NewStdioMCPClientWithOptions(cfg.Command, env, cfg.Args, transport.WithCommandFunc(func(ctx context.Context, command string, env, args []string) (*exec.Cmd, error) {
			cmd := exec.CommandContext(ctx, command, args...)
			cmd.Env = append(os.Environ(), env...)
			cmd.Dir = cfg.CWD
			configureProcess(cmd)
			s.process = cmd
			return cmd, nil
		}), transport.WithCommandLogger(logger))
		if err == nil {
			if stderr, ok := client.GetStderr(c); ok {
				// stderr reaches EOF when the process exits.
				go func() {
					_, _ = io.Copy(io.Discard, stderr)
					if !s.closing.Load() {
						s.lost.Store(true)
					}
				}()
			}
		}
	case "streamable-http":
		// The listening stream carries server notifications such as tool-list
		// changes. Servers that do not offer it answer 405 and are skipped.
		s.http = newRetryTransport()
		options := []transport.StreamableHTTPCOption{transport.WithHTTPHeaders(cfg.Headers), transport.WithHTTPLogger(logger), transport.WithHTTPBasicClient(&http.Client{Transport: s.http}), transport.WithContinuousListening()}
		if useOAuth {
			oauth, oerr := m.oauthTransportConfig(s.name, cfg, "")
			if oerr != nil {
				return nil, nil, oerr
			}
			options = append(options, transport.WithHTTPOAuth(oauth))
		}
		c, err = client.NewStreamableHttpClient(cfg.URL, options...)
		if err == nil {
			// The listener lives as long as the connection, not the
			// connect deadline. shutdown cancels it.
			lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
			s.cancel = cancel
			err = c.Start(lifetime)
		}
	case "sse":
		options := []transport.ClientOption{transport.WithHeaders(cfg.Headers), transport.WithSSELogger(logger)}
		if useOAuth {
			oauth, oerr := m.oauthTransportConfig(s.name, cfg, "")
			if oerr != nil {
				return nil, nil, oerr
			}
			options = append(options, transport.WithOAuth(oauth))
		}
		c, err = client.NewSSEMCPClient(cfg.URL, options...)
		if err == nil {
			c.OnConnectionLost(func(error) {
				if !s.closing.Load() {
					s.lost.Store(true)
				}
			})
			lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
			s.cancel = cancel
			stop := context.AfterFunc(ctx, cancel)
			defer stop()
			err = c.Start(lifetime)
		}
	}
	if err != nil {
		if s.cancel != nil {
			s.cancel()
		}
		terminateProcess(s.process)
		if c != nil {
			c.Close()
		}
		return nil, nil, classify(err)
	}
	// Relist when the server announces a changed tool list. Run outside the
	// transport's reader so the relist request can receive its response.
	c.OnNotification(func(notification protocol.JSONRPCNotification) {
		if notification.Method == protocol.MethodNotificationToolsListChanged {
			go m.relist(s)
		}
	})
	success := false
	defer func() {
		if !success {
			if s.cancel != nil {
				s.cancel()
			}
			terminateProcess(s.process)
			c.Close()
		}
	}()
	initialized, err := c.Initialize(ctx, protocol.InitializeRequest{Params: protocol.InitializeParams{ProtocolVersion: protocol.LATEST_PROTOCOL_VERSION, ClientInfo: protocol.Implementation{Name: "zot", Version: "1"}, Capabilities: protocol.ClientCapabilities{}}})
	if err != nil {
		return nil, nil, classify(err)
	}
	s.resources = initialized.Capabilities.Resources != nil
	definitions, err := listTools(ctx, c)
	if err != nil {
		return nil, nil, classify(err)
	}
	success = true
	return c, definitions, nil
}

// classify marks errors that need an OAuth sign-in so the manager can
// offer one instead of a generic failure.
func classify(err error) error {
	if client.IsOAuthAuthorizationRequiredError(err) || errors.Is(err, transport.ErrOAuthAuthorizationRequired) {
		return fmt.Errorf("%w: %v", ErrSignInRequired, err)
	}
	return err
}

// toolDefinition keeps the input schema exactly as the server sent it. The
// library's typed schema drops keywords such as anyOf, oneOf, and
// description, and renames definitions, which breaks $ref pointers.
type toolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// maxToolPages bounds pagination against servers that never stop.
const maxToolPages = 1000

// listTools reads tools/list page by page. The library's ListTools follows
// cursors itself without loop protection, so a server repeating a cursor
// would never return.
func listTools(ctx context.Context, c *client.Client) ([]toolDefinition, error) {
	var definitions []toolDefinition
	cursor := ""
	seen := map[string]bool{}
	for page := 0; ; page++ {
		if page == maxToolPages {
			return nil, fmt.Errorf("tool list exceeds %d pages", maxToolPages)
		}
		request := transport.JSONRPCRequest{JSONRPC: protocol.JSONRPC_VERSION, ID: protocol.NewRequestId(fmt.Sprintf("zot-tools-%d", resourceRequestID.Add(1))), Method: string(protocol.MethodToolsList)}
		if cursor != "" {
			request.Params = map[string]string{"cursor": cursor}
		}
		response, err := c.GetTransport().SendRequest(ctx, request)
		if err != nil {
			return nil, err
		}
		if response.Error != nil {
			return nil, response.Error.AsError()
		}
		var result struct {
			Tools      []toolDefinition `json:"tools"`
			NextCursor string           `json:"nextCursor"`
		}
		if err := json.Unmarshal(response.Result, &result); err != nil {
			return nil, fmt.Errorf("invalid tools/list result")
		}
		definitions = append(definitions, result.Tools...)
		if result.NextCursor == "" {
			return definitions, nil
		}
		if seen[result.NextCursor] {
			return nil, fmt.Errorf("repeated tool-list cursor")
		}
		seen[result.NextCursor] = true
		cursor = result.NextCursor
	}
}

// toolSchema returns the server's schema, or an empty object schema when the
// server sent none or a value providers would reject.
func toolSchema(raw json.RawMessage) json.RawMessage {
	var probe map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &probe) != nil || probe == nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return raw
}

// SetCWD changes the directory used for the project configuration and as
// the default stdio working directory. It takes effect on the next Reload.
func (m *Manager) SetCWD(cwd string) {
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	m.mu.Lock()
	m.cwd = cwd
	m.mu.Unlock()
}

func (m *Manager) Tools() []core.Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []core.Tool
	for _, name := range sortedKeys(m.tools) {
		out = append(out, m.tools[name])
	}
	return out
}

func (m *Manager) Close() {
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	m.close()
}

func (m *Manager) close() {
	m.mu.Lock()
	old := m.servers
	m.servers = map[string]*server{}
	m.tools = map[string]core.Tool{}
	m.generation++
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range old {
		if s.client != nil {
			wg.Add(1)
			s.closing.Store(true)
			go func() {
				defer wg.Done()
				s.shutdown()
			}()
		}
	}
	wg.Wait()
}

// openTerminal is replaced in tests.
var openTerminal = func() (io.ReadCloser, error) { return os.Open(terminalPath) }

// readPastedRedirect reads a pasted redirect URL from the controlling
// terminal, for a browser on another machine. It never reads stdin, which
// carries protocol data in RPC mode. Without a terminal the sign-in still
// completes through the browser callback.
func readPastedRedirect(done <-chan struct{}, paste func(string) error) {
	if paste == nil {
		return
	}
	terminal, err := openTerminal()
	if err != nil {
		return
	}
	// Closing the terminal unblocks the read once the sign-in ends.
	var closeOnce sync.Once
	closeTerminal := func() { closeOnce.Do(func() { terminal.Close() }) }
	defer closeTerminal()
	go func() {
		<-done
		closeTerminal()
	}()
	fmt.Fprintln(os.Stderr, "Browser on another machine? Paste the address it shows after signing in, then press enter:")
	scanner := bufio.NewScanner(terminal)
	scanner.Buffer(make([]byte, 0, 4096), 64*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if err := paste(line); err != nil {
			fmt.Fprintln(os.Stderr, err)
			continue
		}
		return
	}
}

// serverErr returns the connection error of a server, if any.
func (m *Manager) serverErr(name string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if s := m.servers[name]; s != nil {
		return s.err
	}
	return nil
}

// Command is the text /mcp command for every mode. The host handles
// /mcp reload itself, and runs this under its lock and refreshes the live
// tool registry afterwards, so subcommands can reconnect servers.
func (m *Manager) Command(ctx context.Context, args string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	fields := strings.Fields(args)
	if len(fields) == 2 {
		var err error
		switch fields[0] {
		case "reconnect":
			err = m.Reconnect(ctx, fields[1])
		case "login":
			// Text modes have no progress view, so the authorization URL is
			// printed to stderr for a browser that did not open.
			err = m.SignIn(ctx, fields[1], func(status SignInStatus) {
				fmt.Fprintf(os.Stderr, "MCP sign-in for %s, if the browser did not open, visit:\n%s\n", fields[1], status.URL)
				go readPastedRedirect(status.Done, status.Paste)
			})
			if err == nil {
				err = m.Reconnect(ctx, fields[1])
			}
		case "logout":
			err = m.SignOut(fields[1])
			if err == nil {
				err = m.Reconnect(ctx, fields[1])
				if errors.Is(m.serverErr(fields[1]), ErrSignInRequired) {
					err = nil
				}
			}
		default:
			return "", fmt.Errorf("usage: /mcp [add|reload|help|reconnect <server>|login <server>|logout <server>]")
		}
		if err != nil && !strings.HasPrefix(err.Error(), "MCP server") {
			return "", err
		}
	} else if len(fields) != 0 && args != "help" {
		return "", fmt.Errorf("usage: /mcp [add|reload|help|reconnect <server>|login <server>|logout <server>]")
	}
	snap := m.Snapshot()
	var out strings.Builder
	out.WriteString("MCP servers\n")
	for _, problem := range snap.FileErrors {
		fmt.Fprintf(&out, "  config %s\n", problem)
	}
	for _, s := range snap.Servers {
		switch s.State {
		case StateConnected:
			fmt.Fprintf(&out, "  %s: connected (%d tools, %s)\n", s.Name, s.ToolCount, s.Exposure)
		case StateDisabled:
			fmt.Fprintf(&out, "  %s: disabled\n", s.Name)
		default:
			fmt.Fprintf(&out, "  %s: %s, %s\n", s.Name, s.State, s.Problem)
		}
	}
	if len(snap.Servers) == 0 {
		out.WriteString("  no servers configured\n")
	}
	fmt.Fprintf(&out, "Config: %s and %s. Run /mcp to manage servers, or /mcp reload after editing a file.", snap.UserPath, snap.ProjectPath)
	return out.String(), nil
}

type remoteTool struct {
	name, description, remoteName, namespace string
	schema                                   json.RawMessage
	server                                   *server
	deferred                                 bool
	manager                                  *Manager
}

func (t *remoteTool) Name() string            { return t.name }
func (t *remoteTool) Description() string     { return t.description }
func (t *remoteTool) Schema() json.RawMessage { return t.schema }
func (t *remoteTool) Deferred() bool          { return t.deferred }
func (t *remoteTool) Namespace() string {
	return "mcp__" + invalidName.ReplaceAllString(t.namespace, "_")
}
func (t *remoteTool) Execute(ctx context.Context, args json.RawMessage, _ func(string)) (core.ToolResult, error) {
	var input map[string]any
	if err := json.Unmarshal(args, &input); err != nil || input == nil {
		return core.ToolResult{}, fmt.Errorf("MCP tool arguments must be a JSON object")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(t.server.cfg.RequestTimeout)*time.Second)
	defer cancel()
	// The agent may hold a tool from an older catalog, for example during
	// a turn that started before a reload, a reconnect, or a tool-list
	// change. Only a tool still in the current catalog may be called, on
	// the server's current connection, so withdrawn or hidden tools stay
	// unreachable.
	t.manager.mu.RLock()
	current, _ := t.manager.tools[t.name].(*remoteTool)
	t.manager.mu.RUnlock()
	if current == nil || current.remoteName != t.remoteName || current.server.closing.Load() {
		return core.ToolResult{}, fmt.Errorf("MCP tool %q is no longer available, check /mcp", t.name)
	}
	s := current.server
	if s.lost.Load() {
		// A dropped connection reconnects on the next call.
		var err error
		if s, err = t.manager.revive(ctx, s); err != nil {
			if ctx.Err() != nil {
				return core.ToolResult{}, ctx.Err()
			}
			return core.ToolResult{}, err
		}
	}
	request := protocol.CallToolRequest{Params: protocol.CallToolParams{Name: t.remoteName, Arguments: input}}
	result, err := s.client.CallTool(ctx, request)
	if err != nil && ctx.Err() == nil && s.cfg.Transport == "streamable-http" && errors.Is(err, transport.ErrSessionTerminated) {
		// The server restarted or expired the session before receiving the
		// call, so it did not run. Start a new session and send it once more.
		s.lost.Store(true)
		if s, err = t.manager.revive(ctx, s); err == nil {
			result, err = s.client.CallTool(ctx, request)
		} else if ctx.Err() == nil {
			return core.ToolResult{}, err
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return core.ToolResult{}, ctx.Err()
		}
		if client.IsOAuthAuthorizationRequiredError(err) || errors.Is(err, transport.ErrOAuthAuthorizationRequired) {
			return core.ToolResult{}, fmt.Errorf("MCP server %q needs sign-in, open /mcp and press l", t.namespace)
		}
		// The client wraps transport failures in *transport.Error, while a
		// JSON-RPC error reply means the server is alive. A failed stdio pipe
		// or SSE stream is marked lost and reconnects on the next call.
		// Streamable HTTP has no persistent link, the next call may succeed.
		var transportErr *transport.Error
		if errors.As(err, &transportErr) && s.cfg.Transport != "streamable-http" {
			s.lost.Store(true)
		}
		// Servers report an unknown tool with a JSON-RPC error. The tool
		// may have been withdrawn on a server that cannot announce it, so
		// read the list again in the background. A withdrawn tool then
		// leaves the catalog and the agent is refreshed.
		if errors.Is(err, protocol.ErrInvalidParams) || errors.Is(err, protocol.ErrMethodNotFound) {
			go t.manager.relist(s)
		}
		return core.ToolResult{}, fmt.Errorf("MCP tool call failed, check /mcp to see the server state")
	}
	out := core.ToolResult{IsError: result.IsError}
	for _, content := range result.Content {
		switch c := content.(type) {
		case protocol.TextContent:
			out.Content = append(out.Content, provider.TextBlock{Text: c.Text})
		case protocol.ImageContent:
			data, err := base64.StdEncoding.DecodeString(c.Data)
			if err != nil {
				return core.ToolResult{}, fmt.Errorf("MCP server returned invalid image data")
			}
			out.Content = append(out.Content, provider.ImageBlock{MimeType: c.MIMEType, Data: data})
		default:
			data, _ := json.Marshal(content)
			out.Content = append(out.Content, provider.TextBlock{Text: string(data)})
		}
	}
	if result.StructuredContent != nil {
		out.StructuredContent, _ = json.Marshal(result.StructuredContent)
	}
	if len(out.Content) == 0 {
		out.Content = []provider.Content{provider.TextBlock{Text: "(empty MCP result)"}}
	}
	return out, nil
}

// searchTool reads the manager's current catalog on each search, so a
// search held from an older catalog never activates withdrawn tools.
type searchTool struct{ manager *Manager }

func (*searchTool) Name() string { return "mcp__search_tools" }
func (*searchTool) Description() string {
	return "Search MCP capabilities and load matching deferred tools before calling them."
}
func (*searchTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":20}},"required":["query"],"additionalProperties":false}`)
}
func (t *searchTool) Execute(ctx context.Context, args json.RawMessage, _ func(string)) (core.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return core.ToolResult{}, err
	}
	var input struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return core.ToolResult{}, fmt.Errorf("invalid MCP search arguments")
	}
	if input.Limit == 0 {
		input.Limit = 8
	}
	if input.Limit < 1 || input.Limit > 20 {
		return core.ToolResult{}, fmt.Errorf("limit must be between 1 and 20")
	}
	terms := strings.Fields(strings.ToLower(input.Query))
	t.manager.mu.RLock()
	catalog := t.manager.tools
	t.manager.mu.RUnlock()
	var matches []string
	if len(terms) > 0 {
		for _, name := range sortedKeys(catalog) {
			if name == t.Name() {
				continue
			}
			tool := catalog[name]
			text := strings.ToLower(name + " " + tool.Description())
			matched := true
			for _, term := range terms {
				if !strings.Contains(text, term) {
					matched = false
					break
				}
			}
			if matched {
				matches = append(matches, name)
			}
			if len(matches) == input.Limit {
				break
			}
		}
	}
	text := "No MCP tools matched. Try broader capability words."
	if len(matches) > 0 {
		text = "Loaded MCP tools:\n" + strings.Join(matches, "\n")
	}
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: text}}, ActivateTools: matches}, nil
}
