package modes

import (
	"context"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/tui"
)

func TestInteractivePreservesNativeMouse(t *testing.T) {
	for _, legacyEnabled := range []bool{false, true} {
		term := &cleanupTestTerminal{}
		i := NewInteractive(InteractiveConfig{Terminal: term, TUIClickToPosition: legacyEnabled})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = i.Run(ctx)
		if strings.Contains(term.String(), tui.SeqMouseOn) {
			t.Fatal("startup intercepted native terminal scrolling")
		}
		if !strings.Contains(term.String(), tui.SeqMouseOff) {
			t.Fatal("mouse reporting was not reset")
		}
	}
}

func TestNativeMouseEventsDoNotBrowseHistory(t *testing.T) {
	for _, sideChat := range []bool{false, true} {
		i := NewInteractive(InteractiveConfig{Terminal: &cleanupTestTerminal{}, Theme: tui.Dark, TUIClickToPosition: true})
		if sideChat {
			i.btwDialog = scrollTestBtw()
		}
		i.redraw()
		for _, kind := range []tui.KeyKind{tui.KeyMouseWheelUp, tui.KeyMouseWheelDown, tui.KeyMouseLeft, tui.KeyMouseDrag, tui.KeyMouseRelease} {
			i.handleKey(context.Background(), tui.Key{Kind: kind, MouseX: 3, MouseY: 3})
			if i.scrollOffset != 0 || i.rend.MouseSelectionActive() {
				t.Fatal("mouse event started application history or selection")
			}
		}
		i.openSettingsDialog()
		for _, item := range i.settingsDialog.items {
			for _, child := range item.children {
				if child.key == "tui_click_to_position" {
					t.Fatal("removed mouse capture setting is still offered")
				}
			}
		}
	}
}
