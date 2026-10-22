package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequestTimeoutAndInFlightCancellation(t *testing.T) {
	for _, mode := range []string{"timeout", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{}, 1)
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req rpcRequest
				if json.NewDecoder(r.Body).Decode(&req) != nil {
					w.WriteHeader(400)
					return
				}
				if len(req.ID) == 0 {
					w.WriteHeader(202)
					return
				}
				if req.Method == "tools/call" {
					started <- struct{}{}
					<-r.Context().Done()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": fixtureResult(req)})
			}))
			defer service.Close()
			home, project := t.TempDir(), t.TempDir()
			writeConfig(t, filepath.Join(home, "mcp.json"), fmt.Sprintf(`{"mcpServers":{"slow":{"url":%q,"requestTimeout":1}}}`, service.URL))
			m := New(home, project)
			defer m.Close()
			if errs := m.Reload(context.Background()); len(errs) > 0 {
				t.Fatal(errs)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := registry(m)["mcp__slow__issues"].Execute(ctx, json.RawMessage(`{}`), nil)
				done <- err
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("request not started")
			}
			expected := context.DeadlineExceeded
			if mode == "cancel" {
				expected = context.Canceled
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, expected) {
					t.Fatalf("error=%v want=%v", err, expected)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("request did not stop")
			}
		})
	}
}

func TestSSEConnectionSurvivesDiscoveryDeadline(t *testing.T) {
	responses := make(chan []byte, 8)
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: endpoint\ndata: /messages\n\n")
			w.(http.Flusher).Flush()
			for {
				select {
				case data := <-responses:
					fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		}
		var req rpcRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(400)
			return
		}
		if len(req.ID) > 0 {
			data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": fixtureResult(req)})
			select {
			case responses <- data:
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer service.Close()
	home, project := t.TempDir(), t.TempDir()
	writeConfig(t, filepath.Join(home, "mcp.json"), fmt.Sprintf(`{"mcpServers":{"legacy":{"transport":"sse","url":%q}}}`, service.URL))
	m := New(home, project)
	defer m.Close()
	if errs := m.Reload(context.Background()); len(errs) > 0 {
		t.Fatal(errs)
	}
	// connect's discovery context has already been cancelled at this point.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := registry(m)["mcp__legacy__repo_lookup"].Execute(ctx, json.RawMessage(`{}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

// A rate-limited server's Retry-After is honored, capped, and never
// shortens the default backoff.
func TestRetryDelayHonorsRetryAfter(t *testing.T) {
	base := 300 * time.Millisecond
	with := func(value string) *http.Response {
		resp := &http.Response{Header: http.Header{}}
		if value != "" {
			resp.Header.Set("Retry-After", value)
		}
		return resp
	}
	if got := retryDelay(base, 2, nil); got != 2*base {
		t.Fatalf("no response: %v", got)
	}
	if got := retryDelay(base, 1, with("")); got != base {
		t.Fatalf("no header: %v", got)
	}
	if got := retryDelay(base, 1, with("2")); got != 2*time.Second {
		t.Fatalf("seconds: %v", got)
	}
	if got := retryDelay(base, 1, with("0")); got != base {
		t.Fatalf("zero must not shorten the backoff: %v", got)
	}
	if got := retryDelay(base, 1, with("3600")); got != maxRetryAfter {
		t.Fatalf("not capped: %v", got)
	}
	if got := retryDelay(base, 1, with("garbage")); got != base {
		t.Fatalf("invalid header: %v", got)
	}
	date := time.Now().Add(5 * time.Second).UTC().Format(http.TimeFormat)
	if got := retryDelay(base, 1, with(date)); got < 3*time.Second || got > 5*time.Second {
		t.Fatalf("HTTP date: %v", got)
	}
	// The transport itself waits for the header before retrying.
	var attempts atomic.Int32
	var first, second atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch attempts.Add(1) {
		case 1:
			first.Store(time.Now().UnixNano())
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			second.Store(time.Now().UnixNano())
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	resp, err := newRetryTransport().RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	resp.Body.Close()
	if waited := time.Duration(second.Load() - first.Load()); attempts.Load() != 2 || waited < time.Second {
		t.Fatalf("attempts=%d waited=%v", attempts.Load(), waited)
	}
}

func TestFailureDiagnosticsDoNotExposeCredentials(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	writeConfig(t, filepath.Join(home, "mcp.json"), `{"mcpServers":{"broken":{"url":"http://127.0.0.1:0/secret-token","headers":{"Authorization":"secret-header"},"connectTimeout":1},"off":{"enabled":false}}}`)
	m := New(home, project)
	defer m.Close()
	errs := m.Reload(context.Background())
	if len(errs) != 1 || strings.Contains(errs[0].Error(), "secret") {
		t.Fatalf("errors=%v", errs)
	}
	status, err := m.Command(context.Background(), "")
	if err != nil || strings.Contains(status, "secret") || !strings.Contains(status, "broken") || !strings.Contains(status, "off: disabled") {
		t.Fatalf("status=%s err=%v", status, err)
	}
}
