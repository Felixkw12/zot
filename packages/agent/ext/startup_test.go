package ext

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/patriceckhart/zot/packages/agent/extproto"
)

func TestNotifyStartupFrame(t *testing.T) {
	var out bytes.Buffer
	e := New("test", "1.0")
	e.out = &out
	e.NotifyStartup("success", "connected")
	var frame extproto.NotifyFromExt
	if err := json.Unmarshal(out.Bytes(), &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Type != "notify" || !frame.Startup || frame.Level != "success" || frame.Message != "connected" {
		t.Fatalf("unexpected startup frame: %+v", frame)
	}
	out.Reset()
	e.Notify("info", "runtime")
	var raw map[string]any
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, exists := raw["startup"]; exists {
		t.Fatal("ordinary notifications changed their wire format")
	}
}
