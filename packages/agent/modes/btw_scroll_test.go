package modes

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/tui"
)

func scrollTestBtw() *btwDialog {
	var reply strings.Builder
	for n := 0; n < 100; n++ {
		fmt.Fprintf(&reply, "side row %03d\n\n", n)
	}
	return &btwDialog{
		active: true, theme: tui.Dark, editor: tui.NewEditor(""),
		toolView: &tui.View{Theme: tui.Dark},
		turns:    []btwTurn{{User: "side question", Assistant: reply.String()}},
	}
}

func TestBtwUnboundedTerminalFlow(t *testing.T) {
	d := scrollTestBtw()
	d.editor.SetValue("draft\nsecond line")
	for _, width := range []int{80, 24, 100} {
		chat, bottom, row, _ := d.RenderFlow(tui.Dark, width)
		plain := strings.Join(chat, "\n")
		if len(chat) < 100 {
			t.Fatalf("conversation was capped to %d rows", len(chat))
		}
		for n := 0; n < 100; n++ {
			if !strings.Contains(plain, fmt.Sprintf("side row %03d", n)) {
				t.Fatalf("flow lost row %d at width %d", n, width)
			}
		}
		if len(bottom) > 10 || strings.Contains(strings.Join(bottom, "\n"), "side row") {
			t.Fatal("conversation remains in fixed bottom band")
		}
		if row < 0 || row >= len(bottom) || !strings.Contains(stripANSIBytes(bottom[row]), "second line") {
			t.Fatal("flow cursor does not point at editor")
		}
		combined := append(append([]string(nil), chat...), bottom...)
		if strings.Join(combined, "\n") != strings.Join(d.Render(tui.Dark, width), "\n") {
			t.Fatal("flow split lost panel content")
		}
	}
	d.ToggleToolExpansion()
	d.turns[0].Tools = []tui.ToolCallView{{ID: "tool", Name: "bash", Done: true,
		Result: strings.Repeat("tool output\n", 100)}}
	chat, _, _, _ := d.RenderFlow(tui.Dark, 80)
	if strings.Count(strings.Join(chat, "\n"), "tool output") != 100 {
		t.Fatal("expanded tool output was clipped")
	}
}

func TestBtwSharesMainFlowWithoutMouseReporting(t *testing.T) {
	term := &cleanupTestTerminal{}
	i := NewInteractive(InteractiveConfig{Terminal: term, Theme: tui.Dark})
	i.rend.Resize(80, 24)
	i.btwDialog = scrollTestBtw()
	i.redraw()
	output := term.String()
	if !strings.Contains(output, "side row 000") || !strings.Contains(output, "side row 099") {
		t.Fatal("default terminal flow did not emit the complete side chat")
	}
	if strings.Contains(output, tui.SeqMouseOn) {
		t.Fatal("side chat unexpectedly enabled mouse reporting")
	}
	if i.prevChatLen < 100 {
		t.Fatal("side chat was not included in main scrollable flow")
	}
	if strings.Contains(strings.Join(i.cachedChatLocked(80), "\n"), "side row") || len(i.view.Messages) != 0 {
		t.Fatal("side chat contaminated the main transcript or cache")
	}
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyPageUp})
	i.redraw()
	if i.scrollOffset == 0 {
		t.Fatal("page up did not browse shared history")
	}
	before := i.scrollOffset
	i.btwDialog.turns[0].Assistant += "new output\n\n"
	i.redraw()
	if i.scrollOffset <= before {
		t.Fatal("streaming did not anchor shared history")
	}
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc})
	i.redraw()
	if i.btwDialog.Active() || i.scrollOffset != 0 {
		t.Fatal("closing side chat did not restore main flow")
	}
	if strings.Contains(strings.Join(i.cachedChatLocked(80), "\n"), "side row") {
		t.Fatal("side chat persisted in main flow after closing")
	}
}

func TestBtwWheelAndConfirmationShareHistory(t *testing.T) {
	i := NewInteractive(InteractiveConfig{Terminal: &cleanupTestTerminal{}, Theme: tui.Dark, TUIClickToPosition: true})
	i.rend.Resize(80, 24)
	i.btwDialog = scrollTestBtw()
	i.redraw()
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyMouseWheelUp})
	if i.scrollOffset != 3 {
		t.Fatal("wheel did not use the shared transcript scroll position")
	}
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyMouseWheelDown})
	if i.scrollOffset != 0 {
		t.Fatal("wheel down did not return to tail")
	}
	resp := make(chan core.ConfirmDecision, 1)
	i.confirmDialog.Enqueue(&confirmRequest{toolName: "bash", resp: resp})
	i.redraw()
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyPageUp})
	if i.scrollOffset == 0 || !i.confirmDialog.Active() {
		t.Fatal("page up did not preserve pending confirmation")
	}
	select {
	case <-resp:
		t.Fatal("scrolling answered confirmation")
	default:
	}
}
