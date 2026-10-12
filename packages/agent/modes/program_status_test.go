package modes

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/tui"
)

func TestInteractiveProgramStatusOverlays(t *testing.T) {
	var out bytes.Buffer
	i := NewInteractive(InteractiveConfig{})
	i.programStatus = tui.NewProgramStatus(&out, "1", false)
	assertState := func(want string) {
		t.Helper()
		out.Reset()
		i.syncProgramStatusLocked()
		if !strings.Contains(out.String(), "state="+want+":app=zot") {
			t.Fatalf("output = %q, want %s", out.String(), want)
		}
	}
	i.busy = true
	assertState("working")
	i.extPanel.Open("extension", "panel", "private title", []string{"private output"}, "")
	assertState("blocked")
	i.extPanel.Close()
	assertState("working")
	i.confirmDialog.Enqueue(&confirmRequest{toolName: "bash", resp: make(chan core.ConfirmDecision, 1)})
	assertState("blocked")
	i.confirmDialog.CancelAll("cancelled")
	assertState("working")
	i.busy = false
	i.compacting = true
	out.Reset()
	i.syncProgramStatusLocked()
	if out.String() != tui.FormatProgramStatus(tui.ProgramStatusReport{State: "working", Message: "Compacting context"}) {
		t.Fatalf("compaction output = %q", out.String())
	}
	i.compacting = false
	i.programOutcome = "done"
	assertState("done")
	i.dialog.Open(t.TempDir())
	assertState("blocked")
}

func TestInteractiveProgramStatusResults(t *testing.T) {
	for _, tt := range []struct {
		name       string
		err        error
		cancel     bool
		continuing bool
		want       string
	}{
		{"success", nil, false, false, "done"},
		{"failure", errors.New("request failed\r\nprivate detail"), false, false, "error"},
		{"cancel", context.Canceled, true, false, "idle"},
		{"retry", errors.New("private error"), false, true, "working"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			i := NewInteractive(InteractiveConfig{})
			i.programStatus = tui.NewProgramStatus(&out, "1", false)
			i.programStatus.Set("blocked")
			out.Reset()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancel {
				cancel()
			}
			i.finishProgramStatusLocked(ctx, tt.err, tt.continuing)
			message := ""
			if tt.want == "error" {
				message = "request failed"
			}
			if out.String() != tui.FormatProgramStatus(tui.ProgramStatusReport{State: tt.want, Message: message}) {
				t.Fatalf("output = %q", out.String())
			}
		})
	}
}

type programStatusCompletionWriter struct {
	out       *bytes.Buffer
	want      string
	completed chan struct{}
}

func (w *programStatusCompletionWriter) Write(p []byte) (int, error) {
	n, err := w.out.Write(p)
	if err == nil && string(p) == w.want {
		select {
		case w.completed <- struct{}{}:
		default:
		}
	}
	return n, err
}

func TestInteractiveProgramStatusPromptLifecycle(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			fail := outcome == "failure"
			want := "done"
			if fail {
				want = "error"
			} else if outcome == "cancel" {
				want = "idle"
			}
			message := ""
			if fail {
				message = "request failed"
			}
			completion := tui.FormatProgramStatus(tui.ProgramStatusReport{State: want, Message: message})
			var out bytes.Buffer
			writer := &programStatusCompletionWriter{out: &out, want: completion, completed: make(chan struct{}, 1)}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			started := make(chan struct{})
			release := make(chan struct{})
			i := NewInteractive(InteractiveConfig{
				Agent: core.NewAgent(nil, "test", "", nil),
				PromptDriver: func(ctx context.Context, _ *core.Agent, _ string, _ func(core.AgentEvent)) error {
					close(started)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
					if fail {
						return errors.New("request failed\nprivate detail")
					}
					return nil
				},
			})
			i.programStatus = tui.NewProgramStatus(writer, "1", false)
			// Discard the initial idle report, which matches cancellation's outcome.
			select {
			case <-writer.completed:
			default:
			}
			i.startTurn(ctx, "private prompt")
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			i.mu.Lock()
			working := strings.Contains(out.String(), "state=working:app=zot")
			i.mu.Unlock()
			if !working {
				t.Fatal("prompt did not report working")
			}
			if outcome == "cancel" {
				cancel()
			} else {
				close(release)
			}
			// busy clears before session flushing and the final status write.
			// Wait for that write instead of treating idle as full completion.
			select {
			case <-writer.completed:
			case <-time.After(5 * time.Second):
				t.Fatal("prompt did not report completion")
			}
			i.mu.Lock()
			defer i.mu.Unlock()
			if !strings.HasSuffix(out.String(), completion) || strings.Contains(out.String(), "private") {
				t.Fatalf("completion output = %q", out.String())
			}
		})
	}
}

func TestInteractiveProgramStatusAcknowledgementDoesNotEdit(t *testing.T) {
	var out bytes.Buffer
	i := NewInteractive(InteractiveConfig{})
	i.programStatus = tui.NewProgramStatus(&out, "", true)
	i.ed.SetValue("unsent draft")
	i.busy = true
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyProgramStatus})
	if i.ed.Value() != "unsent draft" || !strings.HasSuffix(out.String(), "\x1b]7501;state=working:app=zot\x1b\\") {
		t.Fatalf("reply changed draft or missed state: %q", out.String())
	}
}

type statusTestTerminal struct {
	cleanupTestTerminal
	tty bool
}

func (t *statusTestTerminal) IsTTY() bool { return t.tty }

func TestInteractiveProgramStatusEnvironment(t *testing.T) {
	for _, tt := range []struct {
		name     string
		tmux     string
		screen   string
		term     string
		tty      bool
		override string
		want     string
	}{
		{"automatic", "", "", "xterm", true, "", tui.ProgramStatusQuery},
		{"tmux", "socket", "", "xterm", true, "", ""},
		{"screen", "", "session", "xterm", true, "", ""},
		{"screen term", "", "", "screen-256color", true, "", ""},
		{"tmux term", "", "", "tmux-256color", true, "", ""},
		{"redirected", "", "", "xterm", false, "", ""},
		{"disabled", "", "", "xterm", true, "0", ""},
		{"forced", "socket", "", "tmux", false, "1", "\x1b]7501;state=idle:app=zot\x1b\\"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TMUX", tt.tmux)
			t.Setenv("STY", tt.screen)
			t.Setenv("TERM", tt.term)
			t.Setenv("ZOT_PROGRAM_STATUS", tt.override)
			term := &statusTestTerminal{tty: tt.tty}
			i := NewInteractive(InteractiveConfig{Terminal: term})
			stop := i.startProgramStatus()
			defer stop()
			if term.String() != tt.want {
				t.Fatalf("startup output = %q, want %q", term.String(), tt.want)
			}
		})
	}
}

func TestInteractiveProgramStatusRunCleanup(t *testing.T) {
	t.Setenv("ZOT_PROGRAM_STATUS", "1")
	term := &cleanupTestTerminal{}
	i := NewInteractive(InteractiveConfig{Terminal: term, Agent: core.NewAgent(nil, "test", "", nil)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = i.Run(ctx)
	if !strings.Contains(term.String(), "\x1b]7501;state=idle:app=zot\x1b\\") || !strings.Contains(term.String(), "\x1b]7501;state=clear\x1b\\") {
		t.Fatalf("missing startup or cleanup status: %q", term.String())
	}
}
