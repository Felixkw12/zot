package modes

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	keymapcfg "github.com/patriceckhart/zot/packages/agent/keymap"
	"github.com/patriceckhart/zot/packages/tui"
)

// keymapBinding is one compiled config.json keymap entry.
type keymapBinding struct {
	name    string // normalized chord name, for example "ctrl+shift+g"
	spec    configuredKey
	command string
	action  string
}

// compileKeymap parses a config.json keymap into bindings sorted by
// chord name. Invalid entries are dropped and reported in issues so the
// interactive mode can surface them instead of silently ignoring them.
func compileKeymap(keymap map[string]string) ([]keymapBinding, []string) {
	return compileKeyBindings(keymapcfg.Commands(keymap))
}

func compileKeyBindings(keymap keymapcfg.Map) (bindings []keymapBinding, issues []string) {
	for _, name := range sortedCustomKeys(keymap) {
		spec, ok := parseConfiguredKey(name)
		if !ok {
			issues = append(issues, fmt.Sprintf("keymap: invalid key %q (use modifier+key, for example ctrl+shift+g)", name))
			continue
		}
		if spec.reserved() {
			issues = append(issues, fmt.Sprintf("keymap: %q is reserved and cannot be rebound", name))
			continue
		}
		value := keymap[name]
		binding := keymapBinding{name: strings.ToLower(strings.TrimSpace(name)), spec: spec}
		if value.Action != "" || value.Context != "" {
			if value.Command != "" || value.Context != "navigation" || (value.Action != "next" && value.Action != "previous") {
				issues = append(issues, fmt.Sprintf("keymap: %q must use action next or previous with context navigation", name))
				continue
			}
			binding.action = value.Action
			bindings = append(bindings, binding)
			continue
		}
		command := strings.TrimSpace(value.Command)
		if command == "" || !strings.HasPrefix(command, "/") {
			issues = append(issues, fmt.Sprintf("keymap: %q must map to a slash command, got %q", name, command))
			continue
		}
		binding.command = command
		bindings = append(bindings, binding)
	}
	return bindings, issues
}

// lookupKeymap resolves key against compiled bindings. An exact chord
// match wins over the legacy Ctrl+letter fallback so enhanced-protocol
// terminals can bind ctrl+g and ctrl+shift+g independently.
func lookupKeymap(bindings []keymapBinding, key tui.Key) string {
	binding, _ := lookupKeyBinding(bindings, key)
	return binding.command
}

func lookupKeyBinding(bindings []keymapBinding, key tui.Key) (keymapBinding, bool) {
	for _, lenient := range []bool{false, true} {
		for _, b := range bindings {
			if b.spec.matches(key, lenient) {
				return b, true
			}
		}
	}
	return keymapBinding{}, false
}

// configuredKeyCommand resolves a configured key chord straight from a
// config.json keymap. Key names are case-insensitive and use '+'
// separated modifiers, for example "ctrl+shift+g", "alt+enter", or
// "cmd+k". Invalid entries are ignored. Interactive mode compiles the
// map once; this helper exists for callers that only have the map.
func configuredKeyCommand(keymap map[string]string, key tui.Key) string {
	if len(keymap) == 0 {
		return ""
	}
	bindings, _ := compileKeymap(keymap)
	return lookupKeymap(bindings, key)
}

// sortedCustomKeys returns keymap names in a stable, case-insensitive
// order for help output and deterministic lookup precedence.
func sortedCustomKeys(keymap keymapcfg.Map) []string {
	keys := make([]string, 0, len(keymap))
	for key := range keymap {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := strings.ToLower(keys[i]), strings.ToLower(keys[j])
		if left == right {
			return keys[i] < keys[j]
		}
		return left < right
	})
	return keys
}

type configuredKey struct {
	kind      tui.KeyKind
	rune      rune
	ctrl, alt bool
	shift     bool
	super     bool
}

func (k configuredKey) modified() bool {
	return k.ctrl || k.alt || k.shift || k.super
}

// reserved reports whether the chord belongs to the emergency/exit
// controls that config.json must never override.
func (k configuredKey) reserved() bool {
	switch {
	case k.kind == tui.KeyRune && k.ctrl && !k.alt && !k.super && (k.rune == 'c' || k.rune == 'd'):
		return true
	case k.kind == tui.KeyEsc && !k.modified():
		return true
	}
	return false
}

var specialConfiguredKeys = map[string]tui.KeyKind{
	"enter": tui.KeyEnter, "return": tui.KeyEnter, "tab": tui.KeyTab,
	"shift-tab": tui.KeyShiftTab, "esc": tui.KeyEsc, "escape": tui.KeyEsc,
	"backspace": tui.KeyBackspace, "delete": tui.KeyDelete,
	"up": tui.KeyUp, "down": tui.KeyDown, "left": tui.KeyLeft, "right": tui.KeyRight,
	"home": tui.KeyHome, "end": tui.KeyEnd, "pageup": tui.KeyPageUp, "pagedown": tui.KeyPageDown,
	"space": tui.KeyRune,
}

func parseConfiguredKey(value string) (configuredKey, bool) {
	var out configuredKey
	parts := strings.Split(strings.ToLower(strings.TrimSpace(value)), "+")
	if len(parts) == 0 {
		return out, false
	}
	base := ""
	for _, part := range parts {
		part = strings.TrimSpace(part)
		switch part {
		case "ctrl", "control":
			out.ctrl = true
		case "alt", "option", "opt":
			out.alt = true
		case "shift":
			out.shift = true
		case "cmd", "command", "super", "meta", "win":
			out.super = true
		case "":
			return out, false
		default:
			if base != "" {
				return out, false
			}
			base = part
		}
	}
	if base == "" {
		return out, false
	}
	out.kind = tui.KeyRune
	if len([]rune(base)) == 1 {
		out.rune = []rune(base)[0]
	} else {
		kind, ok := specialConfiguredKeys[base]
		if !ok {
			return out, false
		}
		// Shift+Tab is represented as a dedicated kind by some terminals
		// and as Tab+Shift by enhanced keyboard protocols; normalize it
		// to the latter so both forms match.
		if kind == tui.KeyShiftTab {
			out.kind = tui.KeyTab
			out.shift = true
		} else {
			out.kind = kind
		}
		if base == "space" {
			out.rune = ' '
		}
	}
	// Unmodified keys belong to normal text entry and navigation. A
	// typo in config.json must not be able to capture Enter, Space,
	// arrows, or a plain letter.
	if !out.modified() {
		return out, false
	}
	return out, true
}

// ctrlKindRunes maps the reader's dedicated control-key kinds back to
// the letter they encode so "ctrl+l" style bindings can match them.
var ctrlKindRunes = map[tui.KeyKind]rune{
	tui.KeyCtrlC: 'c', tui.KeyCtrlD: 'd', tui.KeyCtrlL: 'l', tui.KeyCtrlU: 'u',
	tui.KeyCtrlK: 'k', tui.KeyCtrlA: 'a', tui.KeyCtrlE: 'e', tui.KeyCtrlW: 'w', tui.KeyCtrlO: 'o',
}

// matches reports whether actual is the configured chord. With lenient
// set, a Ctrl+Shift+letter binding also accepts the plain Ctrl+letter
// event because legacy terminal encodings cannot carry Shift on control
// letters. Callers should try exact matching first.
func (k configuredKey) matches(actual tui.Key, lenient bool) bool {
	kind, r, ctrl := actual.Kind, actual.Rune, actual.Ctrl
	shift := actual.Shift
	if kind == tui.KeyShiftTab {
		kind = tui.KeyTab
		shift = true
	}
	// Raw control bytes are exposed as dedicated KeyKinds by the reader.
	if letter, ok := ctrlKindRunes[kind]; ok {
		ctrl = true
		r = letter
		kind = tui.KeyRune
	}
	// Some enhanced keyboard protocols report the shifted glyph rather
	// than the physical number key (Ctrl+Shift+7 becomes '&'). Normalize
	// those common US-layout glyphs so number chords remain bindable.
	if ctrl && shift {
		if digit, ok := shiftedDigit(r); ok {
			r = digit
		}
	}
	// Enhanced terminal protocols preserve the uppercase glyph for shifted
	// letters, while configured key names are case-insensitive.
	r = unicode.ToLower(r)
	wantRune := unicode.ToLower(k.rune)
	shiftMatches := shift == k.shift
	if !shiftMatches && lenient {
		shiftMatches = k.ctrl && k.shift && !shift && ctrl && r >= 'a' && r <= 'z'
	}
	return kind == k.kind && r == wantRune && ctrl == k.ctrl && actual.Alt == k.alt &&
		shiftMatches && actual.Super == k.super
}

func shiftedDigit(r rune) (rune, bool) {
	const glyphs = ")!@#$%^&*("
	if i := strings.IndexRune(glyphs, r); i >= 0 {
		return rune('0' + i), true
	}
	return 0, false
}

// keymapReservedKey reports whether k is one of the emergency/exit
// controls. They stay authoritative at runtime even if a binding somehow
// resolves to them.
func keymapReservedKey(k tui.Key) bool {
	switch k.Kind {
	case tui.KeyCtrlC, tui.KeyCtrlD:
		return true
	case tui.KeyEsc:
		return !k.Ctrl && !k.Alt && !k.Shift && !k.Super
	}
	return false
}

// isChordRune reports whether k is a Ctrl or Super modified rune. The
// reader surfaces unbound chords this way so keymaps can match them;
// they are never typed text and dialogs must not treat them as bare
// letters ('r' renames, 'q' closes, ...).
func isChordRune(k tui.Key) bool {
	return k.Kind == tui.KeyRune && (k.Ctrl || k.Super)
}
