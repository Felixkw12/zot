package tui

import (
	"encoding/base64"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ProgramStatusQuery asks the terminal to acknowledge OSC 7501 support.
const ProgramStatusQuery = "\x1b]7501;?\x1b\\"

// ProgramStatusReport describes the interactive client's root status record.
// Message is limited to a session name, dialog title, or first line of an error.
type ProgramStatusReport struct {
	State   string
	Kind    string // permission, question, or auth, only for blocked reports
	Message string
}

// FormatProgramStatus encodes an OSC 7501 report with safe, bounded metadata.
// Invalid states produce no sequence. Invalid waiting categories are omitted.
func FormatProgramStatus(status ProgramStatusReport) string {
	switch status.State {
	case "clear":
		return "\x1b]7501;state=clear\x1b\\"
	case "idle", "working", "blocked", "done", "error":
	default:
		return ""
	}
	pairs := []string{"state=" + status.State, "app=zot"}
	if status.State == "blocked" {
		switch status.Kind {
		case "permission", "question", "auth":
			pairs = append(pairs, "kind="+status.Kind)
		}
	}
	message := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(status.Message, "\ufffd"))
	message = strings.TrimSpace(message)
	if len(message) > 2048 {
		message = message[:2048]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	if message != "" {
		pairs = append(pairs, "msg="+base64.StdEncoding.EncodeToString([]byte(message)))
	}
	return "\x1b]7501;" + strings.Join(pairs, ":") + "\x1b\\"
}

// ProgramStatus reports activity to supporting terminals.
// The owner must serialize calls with other terminal writes.
type ProgramStatus struct {
	out      io.Writer
	sequence string
	sent     string
	enabled  bool
	pending  bool
	closed   bool
}

// NewProgramStatus starts detection unless override is "1" (force) or "0"
// (disable). Detection should be disabled for non-TTYs and multiplexers.
func NewProgramStatus(out io.Writer, override string, detect bool) *ProgramStatus {
	p := &ProgramStatus{out: out, sequence: FormatProgramStatus(ProgramStatusReport{State: "idle"})}
	p.enabled = override == "1"
	p.pending = override != "1" && override != "0" && detect
	if p.pending {
		_, _ = io.WriteString(out, ProgramStatusQuery)
	}
	p.emit()
	return p
}

// Supported handles a terminal acknowledgement and sends the latest state.
func (p *ProgramStatus) Supported() {
	if p == nil || p.closed || !p.pending {
		return
	}
	p.pending = false
	p.enabled = true
	p.emit()
}

// Set changes the current state. Unknown states are ignored.
func (p *ProgramStatus) Set(state string) {
	if p == nil || p.closed {
		return
	}
	p.Report(ProgramStatusReport{State: state})
}

// Report changes the state and metadata, retaining the latest report during
// detection. Identical encoded reports are sent only once.
func (p *ProgramStatus) Report(status ProgramStatusReport) {
	if p == nil || p.closed || status.State == "clear" {
		return
	}
	sequence := FormatProgramStatus(status)
	if sequence == "" {
		return
	}
	p.sequence = sequence
	p.emit()
}

func (p *ProgramStatus) emit() {
	if p.enabled && p.sent != p.sequence {
		if n, err := io.WriteString(p.out, p.sequence); err == nil && n == len(p.sequence) {
			p.sent = p.sequence
		}
	}
}

// Close clears the terminal's status and prevents subsequent reports.
func (p *ProgramStatus) Close() {
	if p == nil || p.closed {
		return
	}
	if p.enabled {
		_, _ = io.WriteString(p.out, "\x1b]7501;state=clear\x1b\\")
	}
	p.closed = true
}

// readOSC consumes terminal replies so they cannot become editor input.
// A size limit and per-byte timeout bound malformed or incomplete replies.
func (r *Reader) readOSC() (Key, error) {
	var payload strings.Builder
	escape := false
	for n := 0; n < 4096; n++ {
		b, have, err := r.readEscapeNext(50 * time.Millisecond)
		if err != nil || !have {
			return Key{Kind: KeyUnknown}, err
		}
		if b == 7 || (escape && b == '\\') {
			if strings.HasPrefix(payload.String(), "7501;?") {
				return Key{Kind: KeyProgramStatus}, nil
			}
			return Key{Kind: KeyUnknown}, nil
		}
		if escape {
			return Key{Kind: KeyUnknown}, nil
		}
		escape = b == 27
		if !escape {
			payload.WriteByte(b)
		}
	}
	return Key{Kind: KeyUnknown}, nil
}
