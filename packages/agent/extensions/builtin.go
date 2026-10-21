package extensions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/patriceckhart/zot/packages/agent/extproto"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// Builtin is an in-process tool source sharing the host's lifetime, not
// extension reloads. The host handles the reserved "reload" subcommand and
// refreshes the live tool registry. Tools use the normal authorization path.
type Builtin interface {
	Tools() []core.Tool
	Reload(context.Context) []error
	Command(context.Context, string) (string, error)
	Close()
}

// AddBuiltin attaches an already-loaded source. An installed extension with
// the same name takes precedence, allowing existing installations to migrate.
func (m *Manager) AddBuiltin(name string, source Builtin) {
	m.mu.Lock()
	if m.builtins == nil {
		m.builtins = map[string]Builtin{}
	}
	m.builtins[name] = source
	m.mu.Unlock()
}

func (m *Manager) builtinTool(name string) core.Tool {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, exists := m.toolIndex[name]; exists {
		return nil
	}
	for _, owner := range builtinNames(m.builtins) {
		if _, exists := m.ext[owner]; exists {
			continue
		}
		for _, tool := range m.builtins[owner].Tools() {
			if tool.Name() == name {
				return tool
			}
		}
	}
	return nil
}

func invokeBuiltinTool(ctx context.Context, tool core.Tool, args json.RawMessage, timeout time.Duration) (extproto.ToolResultFromExt, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	result, err := tool.Execute(ctx, args, nil)
	out := extproto.ToolResultFromExt{IsError: result.IsError, ActivateTools: result.ActivateTools, StructuredContent: result.StructuredContent}
	for _, content := range result.Content {
		switch block := content.(type) {
		case provider.TextBlock:
			out.Content = append(out.Content, extproto.ContentBlock{Type: "text", Text: block.Text, Format: block.Format})
		case provider.ImageBlock:
			out.Content = append(out.Content, extproto.ContentBlock{Type: "image", MimeType: block.MimeType, Data: base64.StdEncoding.EncodeToString(block.Data)})
		}
	}
	return out, err
}

func builtinNames(sources map[string]Builtin) []string {
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func builtinDescription(name string) string {
	if name == "mcp" {
		return "manage MCP servers: add, enable, exposure, tools, reconnect"
	}
	return "built-in " + name
}

// Builtin returns the active in-process source registered as name, or nil
// when none exists or an installed extension shadows it.
func (m *Manager) Builtin(name string) Builtin {
	if m == nil {
		return nil
	}
	return m.builtinCommand(strings.ToLower(name))
}

func (m *Manager) builtinCommand(name string) Builtin {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, exists := m.commandIndex[name]; exists {
		return nil
	}
	if _, exists := m.ext[name]; exists {
		return nil
	}
	return m.builtins[name]
}

func (m *Manager) stopBuiltins() {
	m.builtinMu.Lock()
	defer m.builtinMu.Unlock()
	m.mu.RLock()
	var sources []Builtin
	for _, source := range m.builtins {
		sources = append(sources, source)
	}
	m.mu.RUnlock()
	for _, source := range sources {
		source.Close()
	}
}

// closeShadowedBuiltins avoids duplicate connections when an installed
// extension takes over a built-in command. Other built-ins are left alone.
func (m *Manager) closeShadowedBuiltins() {
	m.builtinMu.Lock()
	defer m.builtinMu.Unlock()
	m.mu.RLock()
	var shadowed []Builtin
	for name, source := range m.builtins {
		if _, exists := m.ext[name]; exists {
			shadowed = append(shadowed, source)
		}
	}
	m.mu.RUnlock()
	for _, source := range shadowed {
		source.Close()
	}
}

// stalenessRefresher is implemented by built-ins whose tools can change
// without notice, such as MCP servers that cannot announce changes.
type stalenessRefresher interface {
	RefreshStale(context.Context) bool
}

// RefreshBuiltinTools asks built-ins to check for tool changes they could
// not be told about, and refreshes the live tool registry when any
// changed. Hosts call it before the first step of a prompt, so the change
// applies to that prompt. It is a cheap no-op when nothing is due.
func (m *Manager) RefreshBuiltinTools(ctx context.Context) {
	m.mu.RLock()
	var sources []stalenessRefresher
	for name, source := range m.builtins {
		if _, shadowed := m.ext[name]; shadowed {
			continue
		}
		if refresher, ok := source.(stalenessRefresher); ok {
			sources = append(sources, refresher)
		}
	}
	m.mu.RUnlock()
	changed := false
	for _, source := range sources {
		if source.RefreshStale(ctx) {
			changed = true
		}
	}
	if changed {
		m.NotifyBuiltinChange()
	}
}

// NotifyBuiltinChange refreshes the live tool registry after a built-in
// changed its tools on its own, for example when a server announced a new
// tool list. It runs the same callback as a reload.
func (m *Manager) NotifyBuiltinChange() {
	m.lifecycleMu.Lock()
	stopping := m.stopping
	m.lifecycleMu.Unlock()
	if stopping {
		return
	}
	m.mu.RLock()
	callback := m.onReload
	m.mu.RUnlock()
	if callback != nil {
		callback()
	}
}

// ReloadBuiltin runs prepare, then reloads the named source, serialized
// with /<name> reload and shutdown. prepare may retarget the source, for
// example after a working-directory change. The registry callback runs
// afterwards, as for a command reload.
func (m *Manager) ReloadBuiltin(ctx context.Context, name string, prepare func(Builtin)) []error {
	return m.updateBuiltin(ctx, strings.ToLower(name), func(source Builtin) []error {
		if prepare != nil {
			prepare(source)
		}
		return source.Reload(ctx)
	})
}

// UpdateBuiltin runs step against the named source instead of a full
// reload, for example to reconnect one server. It is serialized with
// reloads and shutdown, and refreshes the live tool registry afterwards.
func (m *Manager) UpdateBuiltin(ctx context.Context, name string, step func(Builtin) error) error {
	return errors.Join(m.updateBuiltin(ctx, strings.ToLower(name), func(source Builtin) []error {
		if err := step(source); err != nil {
			return []error{err}
		}
		return nil
	})...)
}

func (m *Manager) reloadBuiltin(ctx context.Context, name string) []error {
	return m.ReloadBuiltin(ctx, name, nil)
}

func (m *Manager) updateBuiltin(ctx context.Context, name string, step func(Builtin) []error) []error {
	m.builtinMu.Lock()
	m.lifecycleMu.Lock()
	stopping := m.stopping
	m.lifecycleMu.Unlock()
	if stopping || ctx.Err() != nil {
		m.builtinMu.Unlock()
		if ctx.Err() != nil {
			return []error{ctx.Err()}
		}
		return []error{fmt.Errorf("host is shutting down")}
	}
	source := m.builtinCommand(name)
	if source == nil {
		m.builtinMu.Unlock()
		return []error{fmt.Errorf("built-in command /%s is unavailable", name)}
	}
	errs := step(source)
	m.mu.RLock()
	callback := m.onReload
	m.mu.RUnlock()
	m.builtinMu.Unlock()
	// Refresh even on partial failure so withdrawn tools do not remain live.
	if callback != nil {
		callback()
	}
	return errs
}

func (m *Manager) invokeBuiltinCommand(ctx context.Context, name, args string, source Builtin) (extproto.CommandResponseFromExt, error) {
	if strings.EqualFold(strings.TrimSpace(args), "reload") {
		if err := errors.Join(m.reloadBuiltin(ctx, name)...); err != nil {
			return extproto.CommandResponseFromExt{}, err
		}
		args = ""
	}
	// Status output is read-only. Subcommands with an argument may
	// reconnect servers, so they run under the same lock as reloads and
	// refresh the tool registry afterwards.
	if len(strings.Fields(args)) < 2 {
		text, err := source.Command(ctx, args)
		return extproto.CommandResponseFromExt{Action: "display", Display: text}, err
	}
	var text string
	err := m.UpdateBuiltin(ctx, name, func(source Builtin) error {
		var commandErr error
		text, commandErr = source.Command(ctx, args)
		return commandErr
	})
	return extproto.CommandResponseFromExt{Action: "display", Display: text}, err
}
