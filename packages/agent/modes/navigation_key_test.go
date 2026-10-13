package modes

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	keymapcfg "github.com/patriceckhart/zot/packages/agent/keymap"
	"github.com/patriceckhart/zot/packages/agent/skills"
	"github.com/patriceckhart/zot/packages/agent/swarm"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

func navigationBindings() keymapcfg.Map {
	return keymapcfg.Map{
		"ctrl+n": {Action: "next", Context: "navigation"},
		"ctrl+p": {Action: "previous", Context: "navigation"},
	}
}

func newNavigationInteractive() *Interactive {
	return NewInteractive(InteractiveConfig{
		Theme:       tui.Dark,
		Terminal:    tui.NewProcTerm(),
		KeyBindings: navigationBindings(),
	})
}

func TestInteractiveControlNavigationDialogs(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(*Interactive) func() int
	}{
		{"model", func(i *Interactive) func() int {
			i.modelDialog.active = true
			i.modelDialog.view = []provider.Model{{ID: "one"}, {ID: "two"}}
			return func() int { return i.modelDialog.cursor }
		}},
		{"rescue", func(i *Interactive) func() int {
			i.rescueDialog.active = true
			i.rescueDialog.view = []provider.Model{{ID: "one"}, {ID: "two"}}
			return func() int { return i.rescueDialog.cursor }
		}},
		{"llama", func(i *Interactive) func() int {
			i.llamaDialog.step = llamaModels
			i.llamaDialog.models = []provider.LlamaCPPModel{{}}
			return func() int { return i.llamaDialog.cursor }
		}},
		{"jump", func(i *Interactive) func() int {
			i.jumpDialog.active = true
			i.jumpDialog.visible = []jumpTarget{{TurnNo: 1}, {TurnNo: 2}}
			return func() int { return i.jumpDialog.cursor }
		}},
		{"session tree", func(i *Interactive) func() int {
			i.sessionTreeDialog.active = true
			i.sessionTreeDialog.items = []sessionTreeItem{{label: "one"}, {label: "two"}}
			return func() int { return i.sessionTreeDialog.cursor }
		}},
		{"swarm", func(i *Interactive) func() int {
			i.swarmDialog.active = true
			i.swarmDialog.snapshot = func() []swarm.AgentSnapshot {
				return []swarm.AgentSnapshot{{ID: "one"}, {ID: "two"}}
			}
			return func() int { return i.swarmDialog.cursor }
		}},
		{"session", func(i *Interactive) func() int {
			i.sessionDialog.active = true
			i.sessionDialog.sessions = []core.SessionSummary{{Path: "one"}, {Path: "two"}}
			return func() int { return i.sessionDialog.cursor }
		}},
		{"settings", func(i *Interactive) func() int {
			i.settingsDialog.Open(settingsTestItems(2))
			return func() int { return i.settingsDialog.cursor }
		}},
		{"session operations", func(i *Interactive) func() int {
			i.sessionOpsDialog.Open([]sessionOpsItem{{action: "timeline"}, {action: "export"}})
			return func() int { return i.sessionOpsDialog.cursor }
		}},
		{"login", func(i *Interactive) func() int {
			i.dialog.step = loginStepMethod
			return func() int { return i.dialog.cursor }
		}},
		{"logout", func(i *Interactive) func() int {
			i.logoutDialog.Open([]logoutItem{{target: "one"}, {target: "two"}})
			return func() int { return i.logoutDialog.cursor }
		}},
		{"telegram", func(i *Interactive) func() int {
			i.telegramDialog.Open([]telegramItem{{action: "connect"}, {action: "status"}})
			return func() int { return i.telegramDialog.cursor }
		}},
		{"skills", func(i *Interactive) func() int {
			i.skillsDialog.Open([]*skills.Skill{{Name: "one"}, {Name: "two"}})
			return func() int { return i.skillsDialog.cursor }
		}},
		{"changelog", func(i *Interactive) func() int {
			i.changelogDialog.Open("test", "", "release notes")
			return func() int { return i.changelogDialog.scroll }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := newNavigationInteractive()
			cursor := tc.open(i)
			for _, step := range []struct {
				key  rune
				want int
			}{{'n', 1}, {'p', 0}, {'p', 0}} {
				i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: step.key, Ctrl: true})
				if got := cursor(); got != step.want {
					t.Fatalf("ctrl+%c cursor = %d, want %d", step.key, got, step.want)
				}
			}
			if len(i.helpBlock) != 0 {
				t.Fatal("main-input keymap ran while dialog owned input")
			}
		})
	}
}

func TestInteractiveControlNavigationConfirmationDoesNotAnswer(t *testing.T) {
	i := newNavigationInteractive()
	resp := enqueueConfirmation(i)
	for _, step := range []struct {
		key  rune
		want int
	}{{'n', 1}, {'p', 0}} {
		i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: step.key, Ctrl: true})
		i.confirmDialog.mu.Lock()
		cursor := i.confirmDialog.cursor
		i.confirmDialog.mu.Unlock()
		if cursor != step.want {
			t.Fatalf("ctrl+%c cursor = %d, want %d", step.key, cursor, step.want)
		}
		select {
		case decision := <-resp:
			t.Fatalf("navigation answered confirmation: %+v", decision)
		default:
		}
	}
}

func TestInteractiveControlNavigationPathChoices(t *testing.T) {
	i := newPathChoiceTestInteractive(t)
	i.keymap, _ = compileKeyBindings(navigationBindings())
	for _, step := range []struct {
		key  rune
		want int
	}{{'n', 1}, {'n', 1}, {'p', 0}} {
		i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: step.key, Ctrl: true})
		if i.pathSuggest.cursor != step.want || !i.pathSuggest.Active(i.ed.Value()) {
			t.Fatalf("ctrl+%c did not navigate the active path popup", step.key)
		}
		if got := i.ed.Value(); got != "./foob" {
			t.Fatalf("navigation changed input to %q", got)
		}
	}
}

func TestInteractiveControlNavigationFilePicker(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"one.txt", "two.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	i := newNavigationInteractive()
	i.fileSuggest.SetCWD(dir)
	i.ed.SetValue("@")
	i.fileSuggest.Render(i.ed.Value(), tui.Dark, 80)
	for _, step := range []struct {
		key  rune
		want int
	}{{'n', 1}, {'n', 1}, {'p', 0}, {'p', 0}} {
		i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: step.key, Ctrl: true})
		if i.fileSuggest.cursor != step.want {
			t.Fatalf("ctrl+%c cursor = %d, want %d", step.key, i.fileSuggest.cursor, step.want)
		}
		if got := i.ed.Value(); got != "@" {
			t.Fatalf("navigation changed input to %q", got)
		}
	}
}
