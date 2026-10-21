package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCancelledPollDoesNotDiscardPendingNotification(t *testing.T) {
	var lists atomic.Int32
	started := make(chan struct{})
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
			if n == 2 {
				close(started)
				<-r.Context().Done()
				return
			}
			name := "initial"
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
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	changed := make(chan struct{}, 1)
	m.OnChange(func() { changed <- struct{}{} })
	s := m.servers["s"]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	go func() {
		m.relistContext(ctx, s, false)
		close(finished)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("poll did not start")
	}
	m.relist(s)
	cancel()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("poll ignored cancellation")
	}
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("pending notification was discarded with the cancelled poll")
	}
	if registry(m)["mcp__s__latest"] == nil {
		t.Fatal("pending notification did not refresh the catalog")
	}
}
