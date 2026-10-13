package modes

import (
	"context"
	"strings"
	"testing"

	keymapcfg "github.com/patriceckhart/zot/packages/agent/keymap"
	"github.com/patriceckhart/zot/packages/tui"
)

func TestCompileKeyBindingsValidatesActions(t *testing.T) {
	bindings, issues := compileKeyBindings(keymapcfg.Map{
		"ctrl+n": {Action: "next", Context: "navigation"},
		"ctrl+p": {Action: "previous", Context: "navigation"},
		"ctrl+s": {Command: "/settings"},
		"ctrl+c": {Action: "next", Context: "navigation"},
		"ctrl+d": {Action: "previous", Context: "navigation"},
		"enter":  {Action: "next", Context: "navigation"},
		"ctrl+x": {Action: "delete", Context: "navigation"},
		"ctrl+y": {Action: "next", Context: "editor"},
		"ctrl+z": {Action: "next"},
		"alt+x":  {Command: "/help", Action: "next", Context: "navigation"},
	})
	if len(bindings) != 3 || len(issues) != 7 {
		t.Fatalf("bindings = %+v, issues = %v, want 3 valid and 7 invalid", bindings, issues)
	}
	for _, b := range bindings {
		if b.command == "" && b.action == "" {
			t.Fatalf("compiled an empty binding: %+v", b)
		}
	}
}

func TestNavigationBindingExactMatchPrecedence(t *testing.T) {
	bindings, issues := compileKeyBindings(keymapcfg.Map{
		"ctrl+n":       {Command: "/help"},
		"ctrl+shift+n": {Action: "next", Context: "navigation"},
	})
	if len(issues) != 0 {
		t.Fatal(issues)
	}
	plain := tui.Key{Kind: tui.KeyRune, Rune: 'n', Ctrl: true}
	if got := navigationKey(bindings, plain); got != plain {
		t.Fatalf("navigation overrode exact command binding: %+v", got)
	}
	shifted := tui.Key{Kind: tui.KeyRune, Rune: 'N', Ctrl: true, Shift: true}
	if got := navigationKey(bindings, shifted); got.Kind != tui.KeyDown {
		t.Fatalf("shifted binding did not navigate: %+v", got)
	}
	fallback, _ := compileKeyBindings(keymapcfg.Map{
		"ctrl+shift+n": {Action: "next", Context: "navigation"},
	})
	if got := navigationKey(fallback, plain); got.Kind != tui.KeyDown {
		t.Fatalf("legacy Ctrl+Shift fallback failed: %+v", got)
	}
}

func TestNavigationHasNoDefaultAliases(t *testing.T) {
	i := newKeymapInteractive(nil)
	i.ed.SetValue("/session ")
	i.suggest.Render(i.ed.Value(), tui.Dark, 80)
	for _, r := range "np" {
		i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: r, Ctrl: true})
	}
	if got := i.suggest.Selection(i.ed.Value()); got != "/session timeline" {
		t.Fatalf("unconfigured keys moved slash selection to %q", got)
	}
	i.ed.Clear()
	i.settingsDialog.Open(settingsTestItems(2))
	for _, r := range "np" {
		i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: r, Ctrl: true})
	}
	if i.settingsDialog.cursor != 0 {
		t.Fatal("unconfigured keys moved the settings cursor")
	}
}

func TestNavigationSupportsCustomChords(t *testing.T) {
	i := NewInteractive(InteractiveConfig{KeyBindings: keymapcfg.Map{
		"alt+j":  {Action: "next", Context: "navigation"},
		"ctrl+l": {Action: "previous", Context: "navigation"},
	}})
	i.settingsDialog.Open(settingsTestItems(2))
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: 'j', Alt: true})
	if i.settingsDialog.cursor != 1 {
		t.Fatal("custom Alt chord did not navigate")
	}
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlL})
	if i.settingsDialog.cursor != 0 {
		t.Fatal("dedicated Ctrl key kind did not navigate")
	}
	i.settingsDialog.Close()
	i.ed.SetValue("draft")
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: 'j', Alt: true})
	if got := i.ed.Value(); got != "draft" {
		t.Fatalf("inactive navigation action typed text: %q", got)
	}
}

func TestNavigationIgnoresTextOnlyLogin(t *testing.T) {
	i := NewInteractive(InteractiveConfig{KeyBindings: keymapcfg.Map{
		"ctrl+w": {Action: "next", Context: "navigation"},
	}})
	i.dialog.step = loginStepLlamaURL
	i.dialog.llamaEd = tui.NewEditor("")
	i.dialog.llamaEd.SetValue("http://localhost")
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlW})
	if got := i.dialog.llamaEd.Value(); got != "http://localhost" {
		t.Fatalf("navigation action changed text-only input: %q", got)
	}
}

func TestHelpShowsNavigationActions(t *testing.T) {
	bindings, issues := compileKeyBindings(navigationBindings())
	if len(issues) != 0 {
		t.Fatal(issues)
	}
	help := strings.Join(renderHelpBlock(tui.Theme{}, 80, false, true, bindings), "\n")
	for _, want := range []string{"ctrl+n", "ctrl+p", "navigation: next", "navigation: previous"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help omitted %q: %s", want, help)
		}
	}
}
