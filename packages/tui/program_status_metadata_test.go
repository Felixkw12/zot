package tui

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestFormatProgramStatusMetadata(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status ProgramStatusReport
		want   string
	}{
		{"permission", ProgramStatusReport{State: "blocked", Kind: "permission", Message: "Allow bash?"}, "\x1b]7501;state=blocked:app=zot:kind=permission:msg=QWxsb3cgYmFzaD8=\x1b\\"},
		{"question", ProgramStatusReport{State: "blocked", Kind: "question"}, "\x1b]7501;state=blocked:app=zot:kind=question\x1b\\"},
		{"auth", ProgramStatusReport{State: "blocked", Kind: "auth"}, "\x1b]7501;state=blocked:app=zot:kind=auth\x1b\\"},
		{"invalid kind", ProgramStatusReport{State: "blocked", Kind: "bad:msg=injected"}, statusSequence("blocked")},
		{"kind only when blocked", ProgramStatusReport{State: "done", Kind: "auth"}, statusSequence("done")},
		{"empty message", ProgramStatusReport{State: "idle", Message: "\n\t\x1b"}, statusSequence("idle")},
		{"invalid state", ProgramStatusReport{State: "bad:msg=injected"}, ""},
		{"clear", ProgramStatusReport{State: "clear", Kind: "auth", Message: "ignored"}, "\x1b]7501;state=clear\x1b\\"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatProgramStatus(tt.status); got != tt.want {
				t.Fatalf("format = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatProgramStatusSanitizesAndBoundsUTF8(t *testing.T) {
	for _, text := range []string{
		"\x1b]7501;state=error\x07\r\n\t\x7f\u009b Grüße",
		strings.Repeat("a", 2047) + "ä",
		strings.Repeat("界", 1000),
		"invalid\xff text",
	} {
		sequence := FormatProgramStatus(ProgramStatusReport{State: "working", Message: text})
		_, message, ok := strings.Cut(sequence, ":msg=")
		if !ok {
			t.Fatal("message missing")
		}
		encoded := strings.TrimSuffix(message, "\x1b\\")
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(decoded) > 2048 || len(encoded) > 2732 || !utf8.Valid(decoded) {
			t.Fatalf("unsafe message: decoded length %d, encoded length %d, error %v", len(decoded), len(encoded), err)
		}
		for _, r := range string(decoded) {
			if unicode.IsControl(r) {
				t.Fatalf("control character %U in decoded message", r)
			}
		}
	}
}

func TestProgramStatusMetadataDetectionAndDeduplication(t *testing.T) {
	var out bytes.Buffer
	p := NewProgramStatus(&out, "", true)
	report := ProgramStatusReport{State: "working", Message: "Named session"}
	p.Report(report)
	if out.String() != ProgramStatusQuery {
		t.Fatal("metadata reported before acknowledgement")
	}
	out.Reset()
	p.Supported()
	if out.String() != FormatProgramStatus(report) {
		t.Fatalf("latest metadata missing: %q", out.String())
	}
	out.Reset()
	p.Report(report)
	if out.Len() != 0 {
		t.Fatal("duplicate metadata emitted")
	}
	report.Message = "Renamed session"
	p.Report(report)
	if out.String() != FormatProgramStatus(report) {
		t.Fatalf("name change not reported: %q", out.String())
	}
}
