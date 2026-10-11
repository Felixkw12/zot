package modes

import (
	"context"
	"os"
	"strings"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/tui"
)

// startProgramStatus runs after raw mode is enabled. All reporter access and
// writes share the interactive mutex with rendering.
func (i *Interactive) startProgramStatus() func() {
	detect := os.Getenv("TMUX") == "" && os.Getenv("STY") == "" && !strings.HasPrefix(os.Getenv("TERM"), "screen") && !strings.HasPrefix(os.Getenv("TERM"), "tmux")
	if tty, ok := i.cfg.Terminal.(interface{ IsTTY() bool }); ok {
		detect = detect && tty.IsTTY()
	}
	i.mu.Lock()
	i.programStatus = tui.NewProgramStatus(i.cfg.Terminal, os.Getenv("ZOT_PROGRAM_STATUS"), detect)
	i.programOutcome = "idle"
	i.programError = ""
	i.programNameLoaded = false
	i.mu.Unlock()
	return func() {
		i.mu.Lock()
		defer i.mu.Unlock()
		i.programStatus.Close()
	}
}

// refreshProgramSessionLocked reads names only on session changes or an
// explicit rename, not on streamed output. Prompt-derived picker titles are
// deliberately excluded from terminal reports.
func (i *Interactive) refreshProgramSessionLocked() {
	path := ""
	if i.cfg.CurrentSessionPath != nil {
		path = i.cfg.CurrentSessionPath()
	}
	if path != i.programSessionPath {
		if i.programNameLoaded {
			i.programOutcome = "idle"
			i.programError = ""
		}
		i.programNameLoaded = false
	}
	if !i.programNameLoaded {
		i.programSessionPath = path
		i.programSessionName = ""
		if path != "" {
			i.programSessionName, _ = core.SessionName(path)
		}
		i.programNameLoaded = true
	}
}

// syncProgramStatusLocked overlays user interaction on the last run outcome.
// Navigation dialogs do not change the activity state.
func (i *Interactive) syncProgramStatusLocked() {
	if i.programStatus == nil {
		return
	}
	i.refreshProgramSessionLocked()
	status := tui.ProgramStatusReport{State: i.programOutcome}
	if status.State == "" {
		status.State = "idle"
	}
	if status.State == "error" {
		status.Message = i.programError
	}
	var confirmationName string
	var confirming, confirmationFocused bool
	if i.confirmDialog != nil {
		i.confirmDialog.mu.Lock()
		if len(i.confirmDialog.pending) > 0 {
			confirming = true
			confirmationName = i.confirmDialog.pending[0].toolName
			confirmationFocused = i.confirmDialog.focused
		}
		i.confirmDialog.mu.Unlock()
	}
	switch {
	case i.dialog.Active():
		status = tui.ProgramStatusReport{State: "blocked", Kind: "auth", Message: "Log in"}
		if i.dialog.provider != "" {
			status.Message += " to " + i.dialog.provider
		}
	case i.extPanel.Active() && !confirmationFocused:
		status = tui.ProgramStatusReport{State: "blocked", Kind: "question", Message: i.extPanel.title}
		if status.Message == "" {
			status.Message = i.extPanel.ext
		}
	case confirming:
		status = tui.ProgramStatusReport{State: "blocked", Kind: "permission", Message: "Allow " + confirmationName + "?"}
	case i.rescueDialog.Active():
		status = tui.ProgramStatusReport{State: "blocked", Kind: "question", Message: "Choose a recovery model"}
	case i.compacting:
		status = tui.ProgramStatusReport{State: "working", Message: "Compacting context"}
	case i.busy:
		status = tui.ProgramStatusReport{State: "working"}
	}
	if (status.State == "working" && !i.compacting) || status.State == "done" {
		status.Message = i.programSessionName
	}
	i.programStatus.Report(status)
}

func (i *Interactive) finishProgramStatusLocked(ctx context.Context, err error, continuing bool) {
	i.programError = ""
	switch {
	case ctx.Err() != nil:
		i.programOutcome = "idle"
	case continuing:
		i.programOutcome = "working"
	case err != nil:
		i.programOutcome = "error"
		i.programError = programErrorFirstLine(err.Error())
	default:
		i.programOutcome = "done"
	}
	i.syncProgramStatusLocked()
}

func programErrorFirstLine(text string) string {
	if end := strings.IndexAny(text, "\r\n"); end >= 0 {
		text = text[:end]
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "Error"
	}
	return text
}
