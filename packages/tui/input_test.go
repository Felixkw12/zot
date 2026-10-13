package tui

import "testing"

func TestReaderParsesCSIUShiftEnter(t *testing.T) {
	k := readKey(t, "\x1b[13;2u")
	if k.Kind != KeyEnter || !k.Shift || k.Alt {
		t.Fatalf("Read kind=%v shift=%v alt=%v, want shift+enter", k.Kind, k.Shift, k.Alt)
	}
}

func TestReaderParsesModifyOtherKeysShiftEnter(t *testing.T) {
	k := readKey(t, "\x1b[27;2;13~")
	if k.Kind != KeyEnter || !k.Shift || k.Alt {
		t.Fatalf("Read kind=%v shift=%v alt=%v, want shift+enter", k.Kind, k.Shift, k.Alt)
	}
}

func TestReaderParsesCSIUCtrlC(t *testing.T) {
	k := readKey(t, "\x1b[99;5u")
	if k.Kind != KeyCtrlC || !k.Ctrl {
		t.Fatalf("Read kind=%v ctrl=%v, want ctrl+c", k.Kind, k.Ctrl)
	}
}

func TestReaderParsesCSIUCtrlNumber(t *testing.T) {
	k := readKey(t, "\x1b[49;5u")
	if k.Kind != KeyRune || k.Rune != '1' || !k.Ctrl {
		t.Fatalf("Read kind=%v rune=%q ctrl=%v, want ctrl+1", k.Kind, k.Rune, k.Ctrl)
	}
}

func TestReaderParsesCSIUSuperNumber(t *testing.T) {
	k := readKey(t, "\x1b[50;9u")
	if k.Kind != KeyRune || k.Rune != '2' || !k.Super {
		t.Fatalf("Read kind=%v rune=%q super=%v, want super+2", k.Kind, k.Rune, k.Super)
	}
}

func TestReaderParsesCSIUSuperNumberWithEventType(t *testing.T) {
	k := readKey(t, "\x1b[51;9:3u")
	if k.Kind != KeyRune || k.Rune != '3' || !k.Super {
		t.Fatalf("Read kind=%v rune=%q super=%v, want super+3", k.Kind, k.Rune, k.Super)
	}
}

func TestReaderParsesCSIUHyperNumberAsSuper(t *testing.T) {
	k := readKey(t, "\x1b[52;33u")
	if k.Kind != KeyRune || k.Rune != '4' || !k.Super {
		t.Fatalf("Read kind=%v rune=%q super=%v, want hyper+4 as super", k.Kind, k.Rune, k.Super)
	}
}

func TestReaderParsesControlNavigationChords(t *testing.T) {
	for _, tc := range []struct {
		sequence string
		want     rune
	}{
		{"\x0e", 'n'},
		{"\x10", 'p'},
		{"\x1b[110;5u", 'n'},
		{"\x1b[112;5u", 'p'},
		{"\x1b[27;5;110~", 'n'},
		{"\x1b[27;5;112~", 'p'},
	} {
		k := readKey(t, tc.sequence)
		if k.Kind != KeyRune || k.Rune != tc.want || !k.Ctrl || k.Alt || k.Shift || k.Super {
			t.Fatalf("Read(%q) = %+v, want plain ctrl+%c", tc.sequence, k, tc.want)
		}
	}
}

func TestReaderParsesRawCtrlGAsModifiedRune(t *testing.T) {
	k := readKey(t, "\x07")
	if k.Kind != KeyRune || k.Rune != 'g' || !k.Ctrl {
		t.Fatalf("Read kind=%v rune=%q ctrl=%v, want ctrl+g", k.Kind, k.Rune, k.Ctrl)
	}
}

func TestReaderParsesRawCtrlSAsModifiedRune(t *testing.T) {
	k := readKey(t, "\x13")
	if k.Kind != KeyRune || k.Rune != 's' || !k.Ctrl {
		t.Fatalf("Read kind=%v rune=%q ctrl=%v, want ctrl+s", k.Kind, k.Rune, k.Ctrl)
	}
}

func TestReaderParsesRawCtrlBAsModifiedRune(t *testing.T) {
	k := readKey(t, "\x02")
	if k.Kind != KeyRune || k.Rune != 'b' || !k.Ctrl {
		t.Fatalf("Read kind=%v rune=%q ctrl=%v, want ctrl+b", k.Kind, k.Rune, k.Ctrl)
	}
}

func TestReaderKeepsDedicatedControlKinds(t *testing.T) {
	for in, want := range map[string]KeyKind{
		"\x01": KeyCtrlA, "\x03": KeyCtrlC, "\x04": KeyCtrlD, "\x05": KeyCtrlE,
		"\x0b": KeyCtrlK, "\x0c": KeyCtrlL, "\x0f": KeyCtrlO, "\x15": KeyCtrlU,
		"\x17": KeyCtrlW, "\x16": KeyPasteClipboard, "\t": KeyTab, "\r": KeyEnter, "\x08": KeyBackspace,
	} {
		if k := readKey(t, in); k.Kind != want {
			t.Fatalf("Read(%q) kind=%v, want %v", in, k.Kind, want)
		}
	}
}

func TestReaderParsesCSIUCtrlLetterAsModifiedRune(t *testing.T) {
	k := readKey(t, "\x1b[115;5u")
	if k.Kind != KeyRune || k.Rune != 's' || !k.Ctrl || k.Shift {
		t.Fatalf("Read = %+v, want ctrl+s", k)
	}
	k = readKey(t, "\x1b[71;6u")
	if k.Kind != KeyRune || k.Rune != 'G' || !k.Ctrl || !k.Shift {
		t.Fatalf("Read = %+v, want ctrl+shift+G", k)
	}
}

func TestReaderParsesRawCtrlVAsClipboardPaste(t *testing.T) {
	k := readKey(t, "\x16")
	if k.Kind != KeyPasteClipboard || !k.Ctrl {
		t.Fatalf("Read kind=%v ctrl=%v, want ctrl+v clipboard paste", k.Kind, k.Ctrl)
	}
}

func TestReaderParsesCSIUCtrlVAsClipboardPaste(t *testing.T) {
	k := readKey(t, "\x1b[118;5u")
	if k.Kind != KeyPasteClipboard || !k.Ctrl {
		t.Fatalf("Read kind=%v ctrl=%v, want enhanced ctrl+v clipboard paste", k.Kind, k.Ctrl)
	}
}

func TestReaderParsesModifyOtherKeysCtrlC(t *testing.T) {
	k := readKey(t, "\x1b[27;5;99~")
	if k.Kind != KeyCtrlC || !k.Ctrl {
		t.Fatalf("Read kind=%v ctrl=%v, want ctrl+c", k.Kind, k.Ctrl)
	}
}

func TestReaderParsesCSIUEsc(t *testing.T) {
	k := readKey(t, "\x1b[27u")
	if k.Kind != KeyEsc {
		t.Fatalf("Read kind=%v, want esc", k.Kind)
	}
}

func TestReaderParsesCSIUTabAndBackspace(t *testing.T) {
	if k := readKey(t, "\x1b[9u"); k.Kind != KeyTab {
		t.Fatalf("Read kind=%v, want tab", k.Kind)
	}
	if k := readKey(t, "\x1b[9;2u"); k.Kind != KeyShiftTab {
		t.Fatalf("Read kind=%v, want shift-tab", k.Kind)
	}
	if k := readKey(t, "\x1b[127u"); k.Kind != KeyBackspace {
		t.Fatalf("Read kind=%v, want backspace", k.Kind)
	}
}

func TestReaderParsesSS3CursorKeys(t *testing.T) {
	cases := []struct {
		seq  string
		want KeyKind
	}{
		{"\x1bOA", KeyUp},
		{"\x1bOB", KeyDown},
		{"\x1bOC", KeyRight},
		{"\x1bOD", KeyLeft},
	}
	for _, tc := range cases {
		if k := readKey(t, tc.seq); k.Kind != tc.want {
			t.Fatalf("Read(%q) kind=%v, want %v", tc.seq, k.Kind, tc.want)
		}
	}
}

func TestReaderParsesSGRMouseWheel(t *testing.T) {
	cases := []struct {
		seq  string
		want KeyKind
	}{
		{"\x1b[<64;10;20M", KeyMouseWheelUp},
		{"\x1b[<65;10;20M", KeyMouseWheelDown},
	}
	for _, tc := range cases {
		k := readKey(t, tc.seq)
		if k.Kind != tc.want {
			t.Fatalf("Read(%q) kind=%v, want %v", tc.seq, k.Kind, tc.want)
		}
	}
}

func readKey(t *testing.T, seq string) Key {
	t.Helper()
	idx := 0
	r := NewReader(func() (byte, error) {
		b := seq[idx]
		idx++
		return b, nil
	})
	k, err := r.Read()
	if err != nil {
		t.Fatalf("Read(%q): %v", seq, err)
	}
	return k
}
