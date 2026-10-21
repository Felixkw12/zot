package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestOverlappingRelistsKeepLatestChanges(t *testing.T) {
	var lists atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req rpcRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := fixtureResult(req)
		if req.Method == "tools/list" {
			n := lists.Add(1)
			name := "initial"
			if n == 2 {
				name = "intermediate"
				close(started)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			if n > 2 {
				name = "latest"
			}
			result = map[string]any{"tools": []any{map[string]any{"name": name, "inputSchema": map[string]any{"type": "object"}}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	m, errs := loadOne(t, srv.URL)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	s := m.servers["s"]
	finished := make(chan struct{})
	go func() {
		m.relist(s)
		close(finished)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("relist did not start")
	}
	// Notifications arriving during a request must queue another read.
	m.relist(s)
	m.relist(s)
	close(release)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("relist did not finish")
	}
	reg := registry(m)
	if reg["mcp__s__latest"] == nil || reg["mcp__s__intermediate"] != nil {
		t.Fatalf("latest change lost: %v", reg)
	}
	if lists.Load() != 3 {
		t.Fatalf("overlapping notifications were not coalesced: %d lists", lists.Load())
	}
}

func TestClosedStderrHelper(t *testing.T) {
	if os.Getenv("ZOT_MCP_CLOSED_STDERR_HELPER") != "1" {
		return
	}
	reader := bufio.NewScanner(os.Stdin)
	writer := json.NewEncoder(os.Stdout)
	for reader.Scan() {
		var req rpcRequest
		if json.Unmarshal(reader.Bytes(), &req) != nil || len(req.ID) == 0 {
			continue
		}
		result := fixtureResult(req)
		if req.Method == "tools/call" {
			_ = os.Stderr.Close()
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": fmt.Sprint(os.Getpid())}}}
		}
		_ = writer.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
	os.Exit(0)
}

func TestClosingStderrDoesNotRestartServer(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	config := map[string]any{"mcpServers": map[string]any{"local": map[string]any{"command": binary, "args": []string{"-test.run=^TestClosedStderrHelper$"}, "env": map[string]string{"ZOT_MCP_CLOSED_STDERR_HELPER": "1"}}}}
	data, _ := json.Marshal(config)
	writeConfig(t, filepath.Join(home, "mcp.json"), string(data))
	m := New(home, t.TempDir())
	t.Cleanup(m.Close)
	if errs := m.Reload(context.Background()); len(errs) > 0 {
		t.Fatal(errs)
	}
	tool := registry(m)["mcp__local__repo_lookup"]
	first, err := tool.Execute(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Give the stderr reader time to observe EOF. The protocol remains live.
	deadline := time.NewTimer(100 * time.Millisecond)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
waiting:
	for {
		select {
		case <-deadline.C:
			break waiting
		case <-tick.C:
			if m.servers["local"].lost.Load() {
				break waiting
			}
		}
	}
	second, err := tool.Execute(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(first.Content) != fmt.Sprint(second.Content) {
		t.Fatalf("healthy server restarted after closing stderr: first=%v second=%v", first.Content, second.Content)
	}
}
