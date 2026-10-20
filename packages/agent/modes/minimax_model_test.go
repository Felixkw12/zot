package modes

import (
	"github.com/patriceckhart/zot/packages/provider"
	"testing"
)

func TestMiniMaxPickerPrefersFlash(t *testing.T) {
	for _, p := range []string{"minimax", "minimax-cn", "openrouter", "opencode"} {
		d := newModelDialog()
		d.Open("", []string{p})
		found := false
		for _, m := range d.all {
			if m.ID == "MiniMax-M3.1-Flash" || m.ID == "minimax/minimax-m3.1-flash:free" || m.ID == "minimax-m3.1-flash-free" {
				found = true
			}
			if (m.ID == "MiniMax-M3" || m.ID == "minimax/minimax-m3" || m.ID == "minimax-m3") && !found {
				t.Errorf("%s picker lists M3 before Flash", p)
			}
		}
		if !found {
			t.Errorf("%s picker missing Flash", p)
		}
	}
	models := sortedModels([]provider.Model{{Provider: "custom", ID: "MiniMax-M3.1-Flash"}, {Provider: "custom", ID: "MiniMax-M3"}})
	if models[0].ID != "MiniMax-M3" {
		t.Fatal("custom provider sort changed")
	}
}
