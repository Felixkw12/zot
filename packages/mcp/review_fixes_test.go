package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReconnectRecordsInvalidConfigurationAndRecovers(t *testing.T) {
	home := t.TempDir()
	path := UserConfigPath(home)
	writeConfig(t, path, `{"mcpServers":{"s":{"enabled":false}}}`)
	m := New(home, t.TempDir())
	t.Cleanup(m.Close)
	if errs := m.Reload(context.Background()); len(errs) != 0 {
		t.Fatal(errs)
	}
	if err := SetServerEnabled(path, "s", true); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconnect(context.Background(), "s"); err == nil {
		t.Fatal("invalid command accepted")
	}
	snap := m.Snapshot()
	if len(snap.Servers) != 1 || snap.Servers[0].State != StateInvalid || !strings.Contains(snap.Servers[0].Problem, "command") || snap.Servers[0].Source != path {
		t.Fatalf("invalid entry not recorded: %+v", snap)
	}
	writeConfig(t, path, `{"mcpServers":{"s":{"enabled":false}}}`)
	if err := m.Reconnect(context.Background(), "s"); err != nil {
		t.Fatal(err)
	}
	snap = m.Snapshot()
	if len(snap.Servers) != 1 || snap.Servers[0].State != StateDisabled {
		t.Fatalf("invalid entry not cleared: %+v", snap)
	}
}

func TestReconnectInvalidConfigurationWithdrawsOldTools(t *testing.T) {
	m, errs := loadOne(t, schemaServer(t, map[string]any{"type": "object"}, nil).URL)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	old := registry(m)["mcp__s__t"]
	writeConfig(t, UserConfigPath(m.home), `{"mcpServers":{"s":{"command":""}}}`)
	if err := m.Reconnect(context.Background(), "s"); err == nil {
		t.Fatal("invalid command accepted")
	}
	if len(m.Tools()) != 0 {
		t.Fatal("invalid server kept old tools")
	}
	if _, err := old.Execute(context.Background(), json.RawMessage(`{}`), nil); err == nil {
		t.Fatal("stale tool remained callable")
	}
}

func TestReviveWaitHonorsCancellation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var initializes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req rpcRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if req.Method == "initialize" && initializes.Add(1) == 2 {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": fixtureResult(req)})
	}))
	t.Cleanup(srv.Close)
	m, errs := loadOne(t, srv.URL)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	s := m.servers["s"]
	s.lost.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first := make(chan error, 1)
	go func() {
		_, err := m.revive(ctx, s)
		first <- err
	}()
	defer func() {
		close(release)
		if err := <-first; err != nil {
			t.Errorf("first reconnect: %v", err)
		}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first reconnect did not start")
	}
	waiting, stop := context.WithCancel(context.Background())
	defer stop()
	second := make(chan error, 1)
	go func() {
		_, err := m.revive(waiting, s)
		second <- err
	}()
	stop()
	select {
	case err := <-second:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting reconnect: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("cancelled caller waited for the other reconnect")
	}
}

func TestNotificationCapabilityRequiredForPollingOptOut(t *testing.T) {
	for _, transport := range []string{"stdio", "sse", "streamable-http"} {
		t.Run(transport, func(t *testing.T) {
			s := &server{cfg: ServerConfig{Transport: transport}, http: newRetryTransport()}
			s.http.streams.Store(1)
			if s.pushesChanges() {
				t.Fatal("notification transport without capability skipped polling")
			}
		})
	}
}

func TestEventStreamWithoutListChangedIsPolled(t *testing.T) {
	fixture := &listChangeServer{tools: []string{"old"}, events: make(chan string), noListChanged: true}
	srv := httptest.NewServer(fixture.handler(t))
	t.Cleanup(srv.Close)
	m, errs := loadOne(t, srv.URL)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	s := m.servers["s"]
	deadline := time.Now().Add(5 * time.Second)
	for !s.http.streaming() {
		if time.Now().After(deadline) {
			t.Fatal("event stream did not open")
		}
		time.Sleep(10 * time.Millisecond)
	}
	fixture.mu.Lock()
	fixture.tools = []string{"new"}
	fixture.mu.Unlock()
	s.listed.Store(0)
	if !m.RefreshStale(context.Background()) {
		t.Fatal("server without listChanged was not polled")
	}
	if _, ok := registry(m)["mcp__s__new"]; !ok {
		t.Fatal("new tool not discovered")
	}
}
