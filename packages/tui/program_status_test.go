package tui

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

func statusSequence(state string) string {
	return "\x1b]7501;state=" + state + ":app=zot\x1b\\"
}

func TestProgramStatusDetection(t *testing.T) {
	var out bytes.Buffer
	p := NewProgramStatus(&out, "", true)
	if out.String() != ProgramStatusQuery {
		t.Fatalf("startup output = %q", out.String())
	}
	p.Set("working")
	p.Set("blocked")
	if out.String() != ProgramStatusQuery {
		t.Fatal("reported before acknowledgement")
	}
	out.Reset()
	p.Supported()
	p.Supported()
	p.Set("blocked")
	if out.String() != statusSequence("blocked") {
		t.Fatalf("acknowledgement output = %q", out.String())
	}
	out.Reset()
	p.Close()
	p.Close()
	p.Set("done")
	p.Supported()
	if out.String() != "\x1b]7501;state=clear\x1b\\" {
		t.Fatalf("close output = %q", out.String())
	}
}

func TestProgramStatusOverrides(t *testing.T) {
	for _, tt := range []struct {
		name     string
		override string
		detect   bool
		want     string
	}{
		{"forced", "1", false, statusSequence("idle")},
		{"disabled", "0", true, ""},
		{"unsupported", "", false, ""},
		{"automatic", "other", true, ProgramStatusQuery + statusSequence("idle")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			p := NewProgramStatus(&out, tt.override, tt.detect)
			p.Supported()
			if out.String() != tt.want {
				t.Fatalf("output = %q, want %q", out.String(), tt.want)
			}
		})
	}
}

func TestProgramStatusStates(t *testing.T) {
	var out bytes.Buffer
	p := NewProgramStatus(&out, "1", true)
	out.Reset()
	for _, state := range []string{"working", "blocked", "done", "error", "idle"} {
		p.Set(state)
		if out.String() != statusSequence(state) {
			t.Fatalf("state %s output = %q", state, out.String())
		}
		out.Reset()
	}
	p.Set("invalid\x1b\\")
	if out.Len() != 0 {
		t.Fatal("invalid state emitted")
	}
}

func TestReaderConsumesOSCReplies(t *testing.T) {
	for _, tt := range []struct {
		sequence string
		kind     KeyKind
	}{
		{ProgramStatusQuery, KeyProgramStatus},
		{"\x1b]7501;?\x07", KeyProgramStatus},
		{"\x1b]7501;?version=2\x1b\\", KeyProgramStatus},
		{"\x1b]7501;state=idle\x07", KeyUnknown},
		{"\x1b]11;rgb:0000/0000/0000\x07", KeyUnknown},
	} {
		t.Run(tt.sequence, func(t *testing.T) {
			input := strings.NewReader(tt.sequence + "x")
			r := NewReader(func() (byte, error) { return input.ReadByte() })
			key, err := r.Read()
			if err != nil || key.Kind != tt.kind {
				t.Fatalf("reply = %#v, %v", key, err)
			}
			key, err = r.Read()
			if err != nil || key.Kind != KeyRune || key.Rune != 'x' {
				t.Fatalf("following key = %#v, %v", key, err)
			}
		})
	}
}

func TestReaderIncompleteOSC(t *testing.T) {
	input := strings.NewReader("\x1b]7501;?")
	r := NewReaderWithPeek(input.ReadByte, func(time.Duration) (byte, bool, error) {
		b, err := input.ReadByte()
		if err == io.EOF {
			return 0, false, nil
		}
		return b, err == nil, err
	})
	key, err := r.Read()
	if err != nil || key.Kind != KeyUnknown {
		t.Fatalf("incomplete reply = %#v, %v", key, err)
	}
}
