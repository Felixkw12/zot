package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ValidServerName reports whether name is accepted as an MCP server key.
func ValidServerName(name string) bool { return serverName.MatchString(name) }

// AddServer writes a new server entry to the configuration file at path.
// Existing servers and unrelated top-level fields are preserved. An existing
// entry with the same name is never overwritten.
func AddServer(path, name string, cfg ServerConfig) error {
	if !ValidServerName(name) {
		return fmt.Errorf("server name may contain only letters, digits, underscores, and hyphens")
	}
	if err := cfg.checkShape(); err != nil {
		return err
	}
	return editServers(path, func(servers map[string]json.RawMessage) error {
		for existing := range servers {
			if invalidName.ReplaceAllString(existing, "_") == invalidName.ReplaceAllString(name, "_") {
				return fmt.Errorf("server %q already exists in %s", existing, path)
			}
		}
		entry, err := json.Marshal(cfg)
		if err != nil {
			return err
		}
		servers[name] = entry
		return nil
	})
}

// SetServerField sets one top-level field of an existing entry, keeping its
// other fields, including ones zot does not know, untouched. A nil value
// removes the field.
func SetServerField(path, name, field string, value any) error {
	return editServers(path, func(servers map[string]json.RawMessage) error {
		raw, ok := servers[name]
		if !ok {
			return fmt.Errorf("server %q is not defined in %s", name, path)
		}
		entry := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &entry); err != nil {
			return fmt.Errorf("server %q in %s is not an object", name, path)
		}
		if value == nil {
			delete(entry, field)
		} else {
			encoded, err := json.Marshal(value)
			if err != nil {
				return err
			}
			entry[field] = encoded
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		servers[name] = encoded
		return nil
	})
}

// SetServerEnabled enables or disables an entry. Enabling removes the field,
// since servers are enabled by default.
//
// A project entry that only disables a user server, {"enabled": false}, is
// deleted when enabled, since an empty entry would be invalid and would
// keep hiding the user server.
func SetServerEnabled(path, name string, enabled bool) error {
	if !enabled {
		return SetServerField(path, name, "enabled", false)
	}
	if err := SetServerField(path, name, "enabled", nil); err != nil {
		return err
	}
	return editServers(path, func(servers map[string]json.RawMessage) error {
		var entry map[string]json.RawMessage
		if json.Unmarshal(servers[name], &entry) == nil && len(entry) == 0 {
			delete(servers, name)
		}
		return nil
	})
}

// SetServerExposure stores the exposure, omitting the deferred default.
func SetServerExposure(path, name, exposure string) error {
	switch exposure {
	case "deferred":
		return SetServerField(path, name, "exposure", nil)
	case "direct", "hidden":
		return SetServerField(path, name, "exposure", exposure)
	}
	return errors.New("exposure must be direct, deferred, or hidden")
}

// RemoveServer deletes an entry from the file at path.
func RemoveServer(path, name string) error {
	return editServers(path, func(servers map[string]json.RawMessage) error {
		if _, ok := servers[name]; !ok {
			return fmt.Errorf("server %q is not defined in %s", name, path)
		}
		delete(servers, name)
		return nil
	})
}

// editServers applies change to the mcpServers object of path and writes
// the file atomically, preserving unrelated top-level fields.
func editServers(path string, change func(map[string]json.RawMessage) error) error {
	root := map[string]json.RawMessage{}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &root); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	}
	servers := map[string]json.RawMessage{}
	if raw, ok := root["mcpServers"]; ok {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return fmt.Errorf("parse %s: mcpServers must be an object", path)
		}
		if servers == nil { // "mcpServers": null
			servers = map[string]json.RawMessage{}
		}
	}
	if err := change(servers); err != nil {
		return err
	}
	if root["mcpServers"], err = json.Marshal(servers); err != nil {
		return err
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(out, '\n'))
}

// CheckServer validates the fields a new entry needs before it is saved.
// Values are not expanded, so environment references stay intact on disk.
func CheckServer(c ServerConfig) error { return c.checkShape() }

func (c ServerConfig) checkShape() error {
	switch {
	case c.URL != "" && c.Command != "":
		return errors.New("choose either a command or a URL")
	case c.URL != "":
		u, err := url.Parse(c.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			if !strings.Contains(c.URL, "${") {
				return errors.New("URL must start with http:// or https://")
			}
		}
	case strings.TrimSpace(c.Command) == "":
		return errors.New("command is required")
	}
	switch c.Exposure {
	case "", "direct", "deferred", "hidden":
	default:
		return errors.New("exposure must be direct, deferred, or hidden")
	}
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	// Write through a symlinked config, such as one kept in a dotfiles
	// repository, instead of replacing the link with a regular file.
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	// Keep the permissions of an existing file. New files are private
	// because they may reference credentials.
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".mcp-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("save %s: %w", path, err)
	}
	return nil
}

// SplitCommandLine splits a command line into words. Single and double quotes
// group words, and a backslash escapes the next character outside single
// quotes. No shell expansion is performed.
func SplitCommandLine(line string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	escaped := false
	for _, r := range line {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped, inWord = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 || escaped {
		return nil, errors.New("unterminated quote or escape")
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}
