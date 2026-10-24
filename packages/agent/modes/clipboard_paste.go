package modes

import (
	"strings"

	"github.com/patriceckhart/zot/packages/tui"
)

type clipboardTextReader func() (string, bool, error)

// resolveClipboardText turns an application-level ctrl+v event into the same
// KeyPaste event terminals produce for bracketed paste. Keeping that conversion
// before dialog routing makes clipboard paste available to every text input.
func resolveClipboardText(k tui.Key, read clipboardTextReader) (tui.Key, bool, error) {
	if k.Kind != tui.KeyPasteClipboard {
		return k, false, nil
	}
	text, ok, err := read()
	if err != nil || !ok || text == "" {
		return k, false, err
	}
	return tui.Key{Kind: tui.KeyPaste, Paste: text}, true, nil
}

// handleEditorKey keeps shell input literal instead of treating pasted paths
// as chat attachments. A bare ! counts while composing, but !@ is a picker.
func (i *Interactive) handleEditorKey(k tui.Key) bool {
	text := strings.TrimLeft(i.ed.Value(), " \t")
	if k.Kind == tui.KeyPaste && strings.HasPrefix(text, "!") && !strings.HasPrefix(text, "!@") {
		i.ed.Insert(k.Paste)
		return false
	}
	return i.ed.HandleKey(k)
}

func singleLinePaste(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
