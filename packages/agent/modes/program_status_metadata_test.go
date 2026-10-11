package modes

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/tui"
)

func TestInteractiveProgramStatusMetadata(t *testing.T) {
	var out bytes.Buffer
	i := NewInteractive(InteractiveConfig{})
	i.programStatus = tui.NewProgramStatus(&out, "1", false)
	assertReport := func(want tui.ProgramStatusReport) {
		t.Helper()
		out.Reset()
		i.syncProgramStatusLocked()
		if out.String() != tui.FormatProgramStatus(want) {
			t.Fatalf("output = %q, want %#v", out.String(), want)
		}
	}
	i.extPanel.Open("extension", "panel", "Choose a target", []string{"private body"}, "private footer")
	assertReport(tui.ProgramStatusReport{State: "blocked", Kind: "question", Message: "Choose a target"})
	i.extPanel.Close()
	i.confirmDialog.Enqueue(&confirmRequest{toolName: "bash", preview: "private command", resp: make(chan core.ConfirmDecision, 1)})
	assertReport(tui.ProgramStatusReport{State: "blocked", Kind: "permission", Message: "Allow bash?"})
	i.confirmDialog.CancelAll("cancelled")
	i.dialog.Open(t.TempDir())
	i.dialog.provider = "anthropic"
	i.dialog.url = "private login URL"
	assertReport(tui.ProgramStatusReport{State: "blocked", Kind: "auth", Message: "Log in to anthropic"})
	i.dialog.Close()
	i.rescueDialog.active = true
	i.rescueDialog.prompt = "private prompt"
	assertReport(tui.ProgramStatusReport{State: "blocked", Kind: "question", Message: "Choose a recovery model"})
	i.rescueDialog.Close()
	out.Reset()
	i.finishProgramStatusLocked(context.Background(), errors.New("request failed\r\nprivate detail"), false)
	if out.String() != tui.FormatProgramStatus(tui.ProgramStatusReport{State: "error", Message: "request failed"}) {
		t.Fatalf("error report = %q", out.String())
	}
	out.Reset()
	i.finishProgramStatusLocked(context.Background(), nil, false)
	if out.String() != tui.FormatProgramStatus(tui.ProgramStatusReport{State: "done"}) {
		t.Fatalf("successful retry retained error metadata: %q", out.String())
	}
}

func TestInteractiveProgramStatusSessionNamesAndSwitches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"meta","meta":{"id":"test"}}
{"type":"message","message":{"role":"user","content":[{"type":"text","text":"private prompt"}]}}
`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := core.RenameSession(path, "Named session"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	i := NewInteractive(InteractiveConfig{CurrentSessionPath: func() string { return path }})
	i.programStatus = tui.NewProgramStatus(&out, "1", false)
	i.busy = true
	out.Reset()
	i.syncProgramStatusLocked()
	if out.String() != tui.FormatProgramStatus(tui.ProgramStatusReport{State: "working", Message: "Named session"}) {
		t.Fatalf("working report = %q", out.String())
	}
	i.busy = false
	i.finishProgramStatusLocked(context.Background(), nil, false)

	// A rename through the picker must refresh the name without resetting done.
	i.sessionDialog.active = true
	i.sessionDialog.renaming = true
	i.sessionDialog.rename = "Renamed session"
	i.sessionDialog.sessions = []core.SessionSummary{{Path: path, Title: "Named session"}}
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
	out.Reset()
	i.syncProgramStatusLocked()
	if out.String() != tui.FormatProgramStatus(tui.ProgramStatusReport{State: "done", Message: "Renamed session"}) {
		t.Fatalf("rename report = %q", out.String())
	}

	// Switching to an unnamed session clears the previous outcome and name.
	path = filepath.Join(t.TempDir(), "new.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"meta","meta":{"id":"new"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	i.syncProgramStatusLocked()
	if out.String() != tui.FormatProgramStatus(tui.ProgramStatusReport{State: "idle"}) {
		t.Fatalf("session switch report = %q", out.String())
	}
	i.busy = true
	out.Reset()
	i.syncProgramStatusLocked()
	if out.String() != tui.FormatProgramStatus(tui.ProgramStatusReport{State: "working"}) {
		t.Fatalf("unnamed report = %q", out.String())
	}
}

func TestProgramErrorFirstLine(t *testing.T) {
	for _, tt := range []struct{ text, want string }{
		{" failure\nprivate detail", "failure"},
		{"failure\rprivate detail", "failure"},
		{"failure\r\nprivate detail", "failure"},
		{"\nprivate detail", "Error"},
		{"", "Error"},
	} {
		if got := programErrorFirstLine(tt.text); got != tt.want {
			t.Fatalf("first line = %q, want %q", got, tt.want)
		}
	}
}
