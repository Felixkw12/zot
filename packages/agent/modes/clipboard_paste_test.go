package modes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/tui"
)

func TestResolveClipboardTextConvertsClipboardKeyToPaste(t *testing.T) {
	key, resolved, err := resolveClipboardText(tui.Key{Kind: tui.KeyPasteClipboard, Ctrl: true}, func() (string, bool, error) {
		return "pasted\ntext", true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resolved || key.Kind != tui.KeyPaste || key.Paste != "pasted\ntext" {
		t.Fatalf("key = %#v, resolved = %v", key, resolved)
	}
}

func TestResolveClipboardTextPreservesClipboardKeyWithoutText(t *testing.T) {
	original := tui.Key{Kind: tui.KeyPasteClipboard, Ctrl: true}
	key, resolved, err := resolveClipboardText(original, func() (string, bool, error) {
		return "", false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved || key != original {
		t.Fatalf("key = %#v, resolved = %v, want original unresolved key", key, resolved)
	}
}

func TestResolveClipboardTextReturnsReadError(t *testing.T) {
	want := errors.New("clipboard unavailable")
	_, resolved, err := resolveClipboardText(tui.Key{Kind: tui.KeyPasteClipboard}, func() (string, bool, error) {
		return "", false, want
	})
	if resolved || !errors.Is(err, want) {
		t.Fatalf("resolved = %v, err = %v, want %v", resolved, err, want)
	}
}

func TestShellInputPaste(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.MkdirAll(filepath.Join(home, ".cache", "huggingface", "hub"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := "~/.cache/huggingface/hub"
	for _, prefix := range []string{"!ls ", "!", "  !ls "} {
		for _, paste := range []string{path, filepath.Join(home, ".cache", "huggingface", "hub"), "'" + path + "'", `~/some\ path "$HOME"`, strings.Repeat("echo hello\n", 20)} {
			for _, clipboard := range []bool{false, true} {
				t.Run(prefix+paste+"/clipboard="+strconv.FormatBool(clipboard), func(t *testing.T) {
					i := NewInteractive(InteractiveConfig{CWD: home})
					i.ed.SetValue(prefix)
					key := tui.Key{Kind: tui.KeyPaste, Paste: paste}
					if clipboard {
						var err error
						key, _, err = resolveClipboardText(tui.Key{Kind: tui.KeyPasteClipboard}, func() (string, bool, error) {
							return paste, true, nil
						})
						if err != nil {
							t.Fatal(err)
						}
						i.handleEditorKey(key)
					} else {
						i.handleKey(context.Background(), key)
					}
					if got := i.ed.SubmitValue(); got != prefix+paste {
						t.Fatalf("submitted input = %q, want %q", got, prefix+paste)
					}
					if i.shellRunning {
						t.Fatal("paste started a shell command")
					}
				})
			}
		}
	}
	for _, prefix := range []string{"describe ", "!@"} {
		t.Run(prefix, func(t *testing.T) {
			i := NewInteractive(InteractiveConfig{CWD: home})
			i.ed.SetValue(prefix)
			i.handleEditorKey(tui.Key{Kind: tui.KeyPaste, Paste: path})
			if got := i.ed.SubmitValue(); got != prefix+"'"+path+"'" {
				t.Fatalf("non-shell paste = %q", got)
			}
		})
	}
}

func TestSingleLinePaste(t *testing.T) {
	if got := singleLinePaste("  claude\n opus\t5  "); got != "claude opus 5" {
		t.Fatalf("singleLinePaste = %q", got)
	}
}
