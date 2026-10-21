package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
	protocol "github.com/mark3labs/mcp-go/mcp"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// Resource tools let the model list and read MCP resources, such as files,
// documentation pages, or schemas a server offers outside its tools. They
// are registered only while a connected, non-hidden server offers
// resources, and reach only those servers.
const (
	listResourcesTool     = "mcp__list_resources"
	listTemplatesTool     = "mcp__list_resource_templates"
	readResourceTool      = "mcp__read_resource"
	maxResourcePages      = 100
	maxInlineResourceSize = 512 << 10
)

// resourceServers returns the servers the resource tools may reach, and
// the widest exposure among them, which the resource tools take on.
func (m *Manager) resourceServersLocked() (names []string, exposure string) {
	rank := map[string]int{"deferred": 1, "direct": 2}
	for _, name := range sortedKeys(m.servers) {
		s := m.servers[name]
		if s.client == nil || s.err != nil || !s.resources || s.cfg.Exposure == "hidden" {
			continue
		}
		names = append(names, name)
		if rank[s.cfg.Exposure] > rank[exposure] {
			exposure = s.cfg.Exposure
		}
	}
	return names, exposure
}

// addResourceTools registers the resource tools in a catalog under
// construction. Server tools are always named mcp__<server>__<tool>, with
// a double underscore after the server, so they cannot take these names.
func (m *Manager) addResourceTools(tools map[string]core.Tool) {
	names, exposure := m.resourceServersLocked()
	if len(names) == 0 {
		return
	}
	deferred := exposure == "deferred"
	servers := strings.Join(names, ", ")
	tools[listResourcesTool] = &resourceTool{
		name: listResourcesTool, manager: m, deferred: deferred,
		description: "List resources offered by MCP servers (" + servers + "), such as files or documents. Read one with " + readResourceTool + ".",
		schema:      `{"type":"object","properties":{"server":{"type":"string","description":"Server to list. Omit to list all."},"cursor":{"type":"string","description":"Continue a previous listing of one server."}},"additionalProperties":false}`,
		run:         m.listResources,
	}
	tools[listTemplatesTool] = &resourceTool{
		name: listTemplatesTool, manager: m, deferred: deferred,
		description: "List URI templates for MCP resources that servers (" + servers + ") do not list directly. Fill in a template and read it with " + readResourceTool + ".",
		schema:      `{"type":"object","properties":{"server":{"type":"string","description":"Server to list. Omit to list all."},"cursor":{"type":"string","description":"Continue a previous listing of one server."}},"additionalProperties":false}`,
		run:         m.listTemplates,
	}
	tools[readResourceTool] = &resourceTool{
		name: readResourceTool, manager: m, deferred: deferred,
		description: "Read an MCP resource by server (" + servers + ") and URI. Text is returned as text, images as images, other binary content is saved to a temporary file and its path returned.",
		schema:      `{"type":"object","properties":{"server":{"type":"string"},"uri":{"type":"string"}},"required":["server","uri"],"additionalProperties":false}`,
		run:         m.readResource,
	}
}

type resourceTool struct {
	name, description, schema string
	deferred                  bool
	manager                   *Manager
	run                       func(context.Context, json.RawMessage) (core.ToolResult, error)
}

func (t *resourceTool) Name() string            { return t.name }
func (t *resourceTool) Description() string     { return t.description }
func (t *resourceTool) Schema() json.RawMessage { return json.RawMessage(t.schema) }
func (t *resourceTool) Deferred() bool          { return t.deferred }
func (t *resourceTool) Namespace() string       { return "mcp" }
func (t *resourceTool) Execute(ctx context.Context, args json.RawMessage, _ func(string)) (core.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return core.ToolResult{}, err
	}
	return t.run(ctx, args)
}

type resourceArgs struct {
	Server string `json:"server"`
	Cursor string `json:"cursor"`
	URI    string `json:"uri"`
}

func parseResourceArgs(raw json.RawMessage) (resourceArgs, error) {
	var args resourceArgs
	if len(raw) == 0 {
		return args, nil
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return args, errors.New("invalid MCP resource arguments")
	}
	return args, nil
}

// resourceServer returns a server the resource tools may reach, reviving a
// dropped connection. Hidden servers and servers without resources are
// refused, so the tools cannot reach anything the catalog does not offer.
func (m *Manager) resourceServer(ctx context.Context, name string) (*server, error) {
	s, err := m.connectedResourceServer(name)
	if err != nil {
		return nil, err
	}
	if s.lost.Load() {
		if _, err := m.revive(ctx, s); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		// Reconnection can withdraw resources or change the active catalog.
		return m.connectedResourceServer(name)
	}
	return s, nil
}

func (m *Manager) connectedResourceServer(name string) (*server, error) {
	m.mu.RLock()
	allowed, _ := m.resourceServersLocked()
	s := m.servers[name]
	m.mu.RUnlock()
	found := false
	for _, candidate := range allowed {
		found = found || candidate == name
	}
	if !found || s == nil {
		if len(allowed) == 0 {
			return nil, errors.New("no connected MCP server offers resources")
		}
		return nil, fmt.Errorf("MCP server %q does not offer resources, use one of: %s", name, strings.Join(allowed, ", "))
	}
	return s, nil
}

// resourceRequestID keeps IDs of zot's own requests unique across concurrent calls. A
// string prefix keeps them apart from the client library's numeric IDs.
var resourceRequestID atomic.Uint64

// resourceRequest sends a resource request with the server's request
// timeout. Results are decoded from the raw response, because the typed
// client results drop fields such as annotations.
func (m *Manager) resourceRequest(ctx context.Context, s *server, method string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.RequestTimeout)*time.Second)
	defer cancel()
	// A paginated listing may hold a connection replaced by an earlier page.
	// Always resolve the current, permitted server before sending a request.
	s, err := m.resourceServer(ctx, s.name)
	if err != nil {
		return nil, err
	}
	request := transport.JSONRPCRequest{
		JSONRPC: protocol.JSONRPC_VERSION, ID: protocol.NewRequestId(fmt.Sprintf("zot-resources-%d", resourceRequestID.Add(1))), Method: method, Params: params,
	}
	response, err := s.client.GetTransport().SendRequest(ctx, request)
	if err != nil && ctx.Err() == nil && s.cfg.Transport == "streamable-http" && errors.Is(err, transport.ErrSessionTerminated) {
		// A terminated session did not accept this request. Reinitialize and
		// retry once, after checking that resources are still offered.
		s.lost.Store(true)
		if s, err = m.resourceServer(ctx, s.name); err != nil {
			return nil, err
		}
		response, err = s.client.GetTransport().SendRequest(ctx, request)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if requiresSignIn(s.cfg, err) {
			m.markSignInRequired(s, err, true)
			return nil, fmt.Errorf("MCP server %q needs sign-in, open /mcp and press l", s.name)
		}
		var transportErr *transport.Error
		if (errors.As(err, &transportErr) && s.cfg.Transport != "streamable-http") || errors.Is(err, transport.ErrSessionTerminated) {
			s.lost.Store(true)
		}
		return nil, fmt.Errorf("MCP server %q did not answer, check /mcp to see the server state", s.name)
	}
	if response.Error != nil {
		// Server messages are shown to the model so it can correct a URI.
		// They come from the server, not from the configuration.
		return nil, fmt.Errorf("MCP server %q: %s", s.name, response.Error.Message)
	}
	return response.Result, nil
}

// listing is one server's page of resources or templates. Items are kept
// as the server sent them, plus the server name.
type listing struct {
	items      []map[string]any
	nextCursor string
}

func (m *Manager) listPage(ctx context.Context, s *server, method, key, cursor string) (listing, error) {
	var params any
	if cursor != "" {
		params = map[string]string{"cursor": cursor}
	}
	raw, err := m.resourceRequest(ctx, s, method, params)
	if err != nil {
		return listing{}, err
	}
	var page struct {
		NextCursor string `json:"nextCursor"`
	}
	fields := map[string]json.RawMessage{}
	if json.Unmarshal(raw, &fields) != nil || json.Unmarshal(raw, &page) != nil {
		return listing{}, fmt.Errorf("MCP server %q returned an invalid %s result", s.name, method)
	}
	var items []map[string]any
	if data, ok := fields[key]; ok && json.Unmarshal(data, &items) != nil {
		return listing{}, fmt.Errorf("MCP server %q returned an invalid %s result", s.name, method)
	}
	kept := items[:0]
	for _, item := range items {
		if item == nil {
			return listing{}, fmt.Errorf("MCP server %q returned an invalid %s item", s.name, method)
		}
		if isAppResource(item) {
			continue
		}
		delete(item, "icons")
		delete(item, "_meta")
		item["server"] = s.name
		kept = append(kept, item)
	}
	return listing{items: kept, nextCursor: page.NextCursor}, nil
}

// isAppResource reports interactive app content, which zot cannot render.
func isAppResource(item map[string]any) bool {
	uri, _ := item["uri"].(string)
	if template, _ := item["uriTemplate"].(string); uri == "" {
		uri = template
	}
	mimeType, _ := item["mimeType"].(string)
	return strings.HasPrefix(uri, "ui://") || strings.Contains(mimeType, "profile=mcp-app")
}

func (m *Manager) listResources(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	return m.list(ctx, raw, string(protocol.MethodResourcesList), "resources")
}

func (m *Manager) listTemplates(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	return m.list(ctx, raw, string(protocol.MethodResourcesTemplatesList), "resourceTemplates")
}

// list returns one page for a named server, or every page of every server
// when no server is named. A failing server is reported in the result
// without hiding the others.
func (m *Manager) list(ctx context.Context, raw json.RawMessage, method, key string) (core.ToolResult, error) {
	args, err := parseResourceArgs(raw)
	if err != nil {
		return core.ToolResult{}, err
	}
	out := map[string]any{}
	if args.Server != "" {
		s, err := m.resourceServer(ctx, args.Server)
		if err != nil {
			return core.ToolResult{}, err
		}
		page, err := m.listPage(ctx, s, method, key, args.Cursor)
		if err != nil {
			return core.ToolResult{}, err
		}
		out["server"], out[key] = args.Server, nonNil(page.items)
		if page.nextCursor != "" {
			out["nextCursor"] = page.nextCursor
		}
		return jsonResult(out)
	}
	if args.Cursor != "" {
		return core.ToolResult{}, errors.New("cursor requires server")
	}
	m.mu.RLock()
	names, _ := m.resourceServersLocked()
	m.mu.RUnlock()
	var all []map[string]any
	var failures []string
	for _, name := range names {
		s, err := m.resourceServer(ctx, name)
		if err == nil {
			err = m.listAll(ctx, s, method, key, &all)
		}
		if err != nil {
			if ctx.Err() != nil {
				return core.ToolResult{}, ctx.Err()
			}
			failures = append(failures, err.Error())
		}
	}
	out[key] = nonNil(all)
	if len(failures) > 0 {
		sort.Strings(failures)
		out["errors"] = failures
	}
	return jsonResult(out)
}

func (m *Manager) listAll(ctx context.Context, s *server, method, key string, all *[]map[string]any) error {
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < maxResourcePages; page++ {
		result, err := m.listPage(ctx, s, method, key, cursor)
		if err != nil {
			return err
		}
		*all = append(*all, result.items...)
		if result.nextCursor == "" {
			return nil
		}
		if seen[result.nextCursor] {
			return fmt.Errorf("MCP server %q repeated a %s cursor", s.name, method)
		}
		seen[result.nextCursor] = true
		cursor = result.nextCursor
	}
	return fmt.Errorf("MCP server %q has more than %d pages of %s, list it with server and cursor", s.name, maxResourcePages, key)
}

func nonNil(items []map[string]any) []map[string]any {
	if items == nil {
		return []map[string]any{}
	}
	return items
}

func jsonResult(value any) (core.ToolResult, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return core.ToolResult{}, err
	}
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: string(data)}}}, nil
}

func (m *Manager) readResource(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	args, err := parseResourceArgs(raw)
	if err != nil {
		return core.ToolResult{}, err
	}
	if args.Server == "" || strings.TrimSpace(args.URI) == "" {
		return core.ToolResult{}, errors.New("server and uri are required")
	}
	s, err := m.resourceServer(ctx, args.Server)
	if err != nil {
		return core.ToolResult{}, err
	}
	result, err := m.resourceRequest(ctx, s, string(protocol.MethodResourcesRead), map[string]string{"uri": args.URI})
	if err != nil {
		return core.ToolResult{}, err
	}
	var read struct {
		Contents []struct {
			URI      string  `json:"uri"`
			MIMEType string  `json:"mimeType"`
			Text     *string `json:"text"`
			Blob     *string `json:"blob"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(result, &read); err != nil {
		return core.ToolResult{}, fmt.Errorf("MCP server %q returned an invalid resources/read result", s.name)
	}
	var out core.ToolResult
	for _, content := range read.Contents {
		uri := content.URI
		if uri == "" {
			uri = args.URI
		}
		switch {
		case content.Text != nil:
			out.Content = append(out.Content, provider.TextBlock{Text: *content.Text})
		case content.Blob != nil:
			data, err := base64.StdEncoding.DecodeString(*content.Blob)
			if err != nil {
				return core.ToolResult{}, fmt.Errorf("MCP server %q returned invalid binary data for %s", s.name, uri)
			}
			block, err := binaryResource(s.name, uri, content.MIMEType, data)
			if err != nil {
				return core.ToolResult{}, err
			}
			out.Content = append(out.Content, block)
		}
	}
	if len(out.Content) == 0 {
		out.Content = []provider.Content{provider.TextBlock{Text: "(empty MCP resource)"}}
	}
	return out, nil
}

// inlineImageTypes are image formats providers accept inline.
var inlineImageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// binaryResource returns an image inline when providers accept its format
// and size, and saves other binary content to a private temporary file so
// the model can process it with its other tools.
func binaryResource(serverName, uri, mimeType string, data []byte) (provider.Content, error) {
	mimeType = strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0]))
	if inlineImageTypes[mimeType] && len(data) <= maxInlineResourceSize {
		return provider.ImageBlock{MimeType: mimeType, Data: data}, nil
	}
	extension := ".bin"
	if extensions, _ := mime.ExtensionsByType(mimeType); len(extensions) > 0 {
		extension = extensions[0]
	}
	file, err := os.CreateTemp("", "zot-mcp-resource-*"+extension)
	if err != nil {
		return nil, fmt.Errorf("cannot save MCP resource: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(file.Name())
		return nil, fmt.Errorf("cannot save MCP resource: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(file.Name())
		return nil, fmt.Errorf("cannot save MCP resource: %w", err)
	}
	label := mimeType
	if label == "" {
		label = "binary"
	}
	return provider.TextBlock{Text: fmt.Sprintf("Binary resource %s from %s (%s, %d bytes) saved to %s", uri, serverName, label, len(data), filepath.ToSlash(file.Name()))}, nil
}
