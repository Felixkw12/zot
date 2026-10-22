package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// retryTransport retries HTTP requests after network errors and transient
// statuses (408, 429, 5xx). tools/call is never retried, because the server
// may already have run the tool. Other MCP requests, such as initialize and
// tools/list, are safe to repeat.
type retryTransport struct {
	base    http.RoundTripper
	retries int
	delay   time.Duration
	// streams counts open server event streams. A server without one
	// cannot push tool-list changes, so the manager reads its list again
	// before turns instead.
	streams atomic.Int32
}

// streaming reports whether the server currently keeps an event stream
// open for notifications.
func (t *retryTransport) streaming() bool { return t.streams.Load() > 0 }

// streamBody decrements the open-stream count once the body closes.
type streamBody struct {
	io.ReadCloser
	once sync.Once
	done func()
}

func (b *streamBody) Close() error {
	b.once.Do(b.done)
	return b.ReadCloser.Close()
}

func newRetryTransport() *retryTransport {
	return &retryTransport{base: http.DefaultTransport, retries: 2, delay: 300 * time.Millisecond}
}

// maxRetryAfter caps a server's Retry-After so a long wait is bounded by
// the caller's deadline rather than the header.
const maxRetryAfter = 30 * time.Second

// retryDelay returns how long to wait before the given attempt. A server's
// Retry-After header, in seconds or as an HTTP date, overrides the default
// backoff when it asks for a longer wait.
func retryDelay(base time.Duration, attempt int, resp *http.Response) time.Duration {
	delay := base * time.Duration(attempt)
	if resp == nil {
		return delay
	}
	header := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if header == "" {
		return delay
	}
	var wait time.Duration
	if seconds, err := strconv.Atoi(header); err == nil && seconds >= 0 {
		wait = time.Duration(seconds) * time.Second
	} else if at, err := http.ParseTime(header); err == nil {
		wait = time.Until(at)
	}
	if wait > maxRetryAfter {
		wait = maxRetryAfter
	}
	if wait > delay {
		return wait
	}
	return delay
}

func transientStatus(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Only JSON-RPC POSTs are retried. GET event streams and DELETEs pass
	// through unchanged.
	if req.Method == http.MethodGet {
		resp, err := t.base.RoundTrip(req)
		if err == nil && resp.StatusCode == http.StatusOK && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			t.streams.Add(1)
			resp.Body = &streamBody{ReadCloser: resp.Body, done: func() { t.streams.Add(-1) }}
		}
		return resp, err
	}
	if req.Method != http.MethodPost || req.Body == nil {
		return t.base.RoundTrip(req)
	}
	body, err := io.ReadAll(req.Body)
	req.Body.Close()
	if err != nil {
		return nil, err
	}
	attempts := 1
	if retryable(body) {
		attempts += t.retries
	}
	var resp *http.Response
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(retryDelay(t.delay, attempt, resp))
			select {
			case <-req.Context().Done():
				timer.Stop()
				return nil, req.Context().Err()
			case <-timer.C:
			}
		}
		clone := req.Clone(req.Context())
		clone.Body = io.NopCloser(bytes.NewReader(body))
		clone.ContentLength = int64(len(body))
		resp, err = t.base.RoundTrip(clone)
		if req.Context().Err() != nil {
			return resp, err
		}
		if err == nil && !transientStatus(resp.StatusCode) {
			return resp, nil
		}
		if attempt < attempts-1 && resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
		}
	}
	return resp, err
}

// retryable reports whether a JSON-RPC request body may be sent again.
// Notifications and responses carry no method worth repeating safely, and
// unknown bodies are not retried.
func retryable(body []byte) bool {
	var message struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	if json.Unmarshal(body, &message) != nil || message.Method == "" || len(message.ID) == 0 {
		return false
	}
	return message.Method != "tools/call"
}
