// Package mcp connects configured Model Context Protocol servers to zot tools.
package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ServerConfig accepts the same transport and timeout fields as the MCP bridge.
type ServerConfig struct {
	Command        string            `json:"command,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Transport      string            `json:"transport,omitempty"`
	Type           string            `json:"type,omitempty"`
	URL            string            `json:"url,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	CWD            string            `json:"cwd,omitempty"`
	Enabled        *bool             `json:"enabled,omitempty"`
	Exposure       string            `json:"exposure,omitempty"`
	ConnectTimeout int               `json:"connectTimeout,omitempty"`
	RequestTimeout int               `json:"requestTimeout,omitempty"`
	Timeout        int               `json:"timeout,omitempty"`
	// ToolExposure overrides Exposure for individual tools. Keys are exact
	// tool names or patterns where * matches any characters.
	ToolExposure map[string]string `json:"toolExposure,omitempty"`
	// OAuth configures sign-in for HTTP servers. Servers that require
	// OAuth work without it, using dynamic client registration.
	OAuth *OAuthConfig `json:"oauth,omitempty"`
}

// OAuthConfig holds optional OAuth settings for servers that need a
// registered client or a specific authorization server.
type OAuthConfig struct {
	ClientID              string   `json:"clientId,omitempty"`
	ClientSecret          string   `json:"clientSecret,omitempty"`
	Scopes                []string `json:"scopes,omitempty"`
	CallbackPort          int      `json:"callbackPort,omitempty"`
	AuthServerMetadataURL string   `json:"authServerMetadataUrl,omitempty"`
}

// overrideFields are the only fields a project entry without a transport
// changes on the user entry of the same name.
var overrideFields = []string{"enabled", "exposure", "toolExposure"}

// transportFields mark a project entry as a full definition.
var transportFields = []string{"command", "url", "type", "transport"}

var envReference = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)
var serverName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var invalidName = regexp.MustCompile(`[^A-Za-z0-9_]`)

// UserConfigPath and ProjectConfigPath are the two configuration files.
func UserConfigPath(home string) string   { return filepath.Join(home, "mcp.json") }
func ProjectConfigPath(cwd string) string { return filepath.Join(cwd, ".zot", "mcp.json") }

func loadConfig(home, cwd string) (map[string]ServerConfig, []error) {
	loaded := loadConfigWithSources(home, cwd)
	return loaded.configs, loaded.errs
}

type loadedConfig struct {
	configs   map[string]ServerConfig
	raw       map[string]ServerConfig // unexpanded, safe to display
	sources   map[string]string       // file defining the effective entry
	overrides map[string]bool         // project entry replaces a user entry
	invalid   map[string]string       // rejected entries and their reason
	fileErrs  []string                // unreadable or malformed files
	errs      []error
}

// loadConfigWithSources also reports which file defines each server and
// which entries were rejected, so the manager can still list them.
func loadConfigWithSources(home, cwd string) loadedConfig {
	entries := map[string]json.RawMessage{}
	sources := map[string]string{}
	overrides := map[string]bool{}
	invalid := map[string]string{}
	raw := map[string]ServerConfig{}
	var fileErrs []string
	var errs []error
	for _, path := range []string{UserConfigPath(home), ProjectConfigPath(cwd)} {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("MCP config %s: %w", path, err))
			fileErrs = append(fileErrs, path+": cannot be read")
			continue
		}
		var file struct {
			Servers map[string]json.RawMessage `json:"mcpServers"`
		}
		if err = json.Unmarshal(data, &file); err != nil {
			errs = append(errs, fmt.Errorf("MCP config %s: %w", path, err))
			fileErrs = append(fileErrs, path+": invalid JSON, fix or remove the file")
			continue
		}
		for name, raw := range file.Servers {
			if previous, exists := entries[name]; exists {
				overrides[name] = true
				raw = mergeOverride(previous, raw)
			}
			entries[name] = raw
			sources[name] = path
		}
	}
	result := map[string]ServerConfig{}
	names := map[string]bool{}
	for _, name := range sortedKeys(entries) {
		var cfg ServerConfig
		err := json.Unmarshal(entries[name], &cfg)
		normalized := invalidName.ReplaceAllString(name, "_")
		if err == nil {
			var unexpanded ServerConfig
			_ = json.Unmarshal(entries[name], &unexpanded)
			raw[name] = unexpanded
		}
		if err != nil {
			invalid[name] = "invalid configuration"
			errs = append(errs, fmt.Errorf("MCP server %q: invalid configuration", name))
			continue
		}
		if !serverName.MatchString(name) || names[normalized] {
			invalid[name] = "invalid or colliding name"
			errs = append(errs, fmt.Errorf("MCP server %q: invalid or colliding name", name))
			continue
		}
		names[normalized] = true
		if cfg.Enabled != nil && !*cfg.Enabled {
			result[name] = cfg
			continue
		}
		if err = cfg.validate(cwd); err != nil {
			invalid[name] = err.Error()
			errs = append(errs, fmt.Errorf("MCP server %q: %w", name, err))
			continue
		}
		result[name] = cfg
	}
	return loadedConfig{configs: result, raw: raw, sources: sources, overrides: overrides, invalid: invalid, fileErrs: fileErrs, errs: errs}
}

// mergeOverride applies a project entry to the user entry it replaces. A
// project entry with its own transport replaces the user entry. Otherwise
// it changes only enabled, exposure, and toolExposure, so a project can
// turn off a server or narrow its tools while keeping the user's command,
// URL, headers, and credentials.
func mergeOverride(user, project json.RawMessage) json.RawMessage {
	var projectFields, userFields map[string]json.RawMessage
	if json.Unmarshal(project, &projectFields) != nil || projectFields == nil {
		return project
	}
	for _, field := range transportFields {
		if _, ok := projectFields[field]; ok {
			return project
		}
	}
	if json.Unmarshal(user, &userFields) != nil || userFields == nil {
		return project
	}
	for _, field := range overrideFields {
		if value, ok := projectFields[field]; ok {
			userFields[field] = value
		}
	}
	merged, err := json.Marshal(userFields)
	if err != nil {
		return project
	}
	return merged
}

func validExposure(value string) bool {
	return value == "direct" || value == "deferred" || value == "hidden"
}

// exposureFor returns the exposure of one tool. An exact toolExposure key
// wins. Among matching patterns the longest, most specific one wins, with
// ties broken lexically so the result does not depend on map order.
func (c *ServerConfig) exposureFor(tool string) string {
	if value, ok := c.ToolExposure[tool]; ok {
		return value
	}
	best := ""
	found := false
	for pattern := range c.ToolExposure {
		if !strings.Contains(pattern, "*") || !matchPattern(pattern, tool) {
			continue
		}
		if !found || len(pattern) > len(best) || (len(pattern) == len(best) && pattern < best) {
			best, found = pattern, true
		}
	}
	if found {
		return c.ToolExposure[best]
	}
	return c.Exposure
}

// matchPattern reports whether name matches pattern, where * matches any
// run of characters, including none.
func matchPattern(pattern, name string) bool {
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	name = name[len(parts[0]):]
	for idx, part := range parts[1:] {
		if idx == len(parts)-2 {
			return strings.HasSuffix(name, part)
		}
		pos := strings.Index(name, part)
		if pos < 0 {
			return false
		}
		name = name[pos+len(part):]
	}
	return name == ""
}

// hasAuthorizationHeader reports whether static credentials are configured,
// in which case OAuth is not used.
func (c *ServerConfig) hasAuthorizationHeader() bool {
	for key := range c.Headers {
		if strings.EqualFold(key, "Authorization") {
			return true
		}
	}
	return false
}

func (c *ServerConfig) validate(cwd string) error {
	var missing []error
	expand := func(value string) string {
		return envReference.ReplaceAllStringFunc(value, func(ref string) string {
			parts := envReference.FindStringSubmatch(ref)
			if v, ok := os.LookupEnv(parts[1]); ok {
				return v
			}
			if parts[2] != "" {
				return parts[3]
			}
			missing = append(missing, fmt.Errorf("environment variable %s is not set", parts[1]))
			return ""
		})
	}
	c.Command = expand(c.Command)
	c.URL = expand(c.URL)
	c.CWD = expand(c.CWD)
	for i, v := range c.Args {
		c.Args[i] = expand(v)
	}
	for k, v := range c.Env {
		c.Env[k] = expand(v)
	}
	for k, v := range c.Headers {
		c.Headers[k] = expand(v)
	}
	if len(missing) > 0 {
		return errors.Join(missing...)
	}
	if c.Transport == "" {
		c.Transport = c.Type
	}
	if c.Transport == "" {
		if c.URL != "" {
			c.Transport = "streamable-http"
		} else {
			c.Transport = "stdio"
		}
	}
	if c.Transport == "http" {
		c.Transport = "streamable-http"
	}
	switch c.Transport {
	case "stdio":
		if strings.TrimSpace(c.Command) == "" || c.URL != "" {
			return errors.New("stdio requires command and no url")
		}
	case "streamable-http", "sse":
		if c.URL == "" || c.Command != "" {
			return errors.New("HTTP transport requires url and no command")
		}
	default:
		return errors.New("unsupported transport")
	}
	if c.Exposure == "" {
		c.Exposure = "deferred"
	}
	if !validExposure(c.Exposure) {
		return errors.New("exposure must be direct, deferred, or hidden")
	}
	for pattern, value := range c.ToolExposure {
		if !validExposure(value) {
			return fmt.Errorf("toolExposure %q must be direct, deferred, or hidden", pattern)
		}
	}
	if c.OAuth != nil {
		if c.Transport == "stdio" {
			return errors.New("oauth applies only to HTTP servers")
		}
		c.OAuth.ClientID = expand(c.OAuth.ClientID)
		c.OAuth.ClientSecret = expand(c.OAuth.ClientSecret)
		c.OAuth.AuthServerMetadataURL = expand(c.OAuth.AuthServerMetadataURL)
		if len(missing) > 0 {
			return errors.Join(missing...)
		}
		if c.OAuth.CallbackPort < 0 || c.OAuth.CallbackPort > 65535 {
			return errors.New("oauth callbackPort must be between 1 and 65535, or 0 for a free port")
		}
	}
	if c.ConnectTimeout == 0 {
		c.ConnectTimeout = 30
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = c.Timeout
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = 60
	}
	if c.ConnectTimeout < 0 || c.RequestTimeout < 0 {
		return errors.New("timeouts must be positive")
	}
	if c.CWD == "" {
		c.CWD = cwd
	} else if !filepath.IsAbs(c.CWD) {
		c.CWD = filepath.Join(cwd, c.CWD)
	}
	return nil
}
