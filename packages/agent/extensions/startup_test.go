package extensions

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

type startupHooks struct {
	stubHooks
	startup []string
}

func (h *startupHooks) NotifyStartup(name, level, message string) {
	h.startup = append(h.startup, name+":"+level+":"+message)
}

func TestStartupNotificationRouting(t *testing.T) {
	for _, supported := range []bool{false, true} {
		legacy := &stubHooks{}
		modern := &startupHooks{}
		var hooks HostHooks = legacy
		if supported {
			hooks = modern
		}
		m := New(t.TempDir(), t.TempDir(), "test", "", "", hooks)
		log, err := os.CreateTemp(t.TempDir(), "extension-log")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { log.Close() })
		ext := &Extension{Manifest: Manifest{Name: "test"}, readyCh: make(chan struct{}), logFile: log}
		frames := `{"type":"notify","level":"success","message":"connected","startup":true}
{"type":"notify","level":"info","message":"runtime"}
`
		m.readLoop(ext, bufio.NewScanner(strings.NewReader(frames)))
		if supported {
			if len(modern.startup) != 1 || modern.startup[0] != "test:success:connected" || len(modern.notifies) != 1 {
				t.Fatalf("unexpected routing: startup=%v runtime=%v", modern.startup, modern.notifies)
			}
		} else if len(legacy.notifies) != 2 {
			t.Fatalf("legacy hook lost notifications: %v", legacy.notifies)
		}
	}
}
