package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionNameExcludesPromptDerivedTitles(t *testing.T) {
	for _, tt := range []struct {
		name string
		rows string
		want string
	}{
		{"unnamed", `{"type":"meta","meta":{"id":"test"}}
{"type":"message","message":{"role":"user","content":[{"type":"text","text":"private prompt"}]}}
`, ""},
		{"metadata", `{"type":"meta","meta":{"id":"test","title":"Named session"}}
`, "Named session"},
		{"legacy rename", `{"type":"meta","meta":{"id":"test"}}
{"type":"rename","title":"First name"}
{"type":"rename","title":"Latest name"}
`, "Latest name"},
		{"branch generated", `{"type":"meta","meta":{"id":"test","parent":"parent"}}
{"type":"rename","title":"private prompt"}
`, ""},
		{"explicit branch", `{"type":"meta","meta":{"id":"test","parent":"parent"}}
{"type":"rename","title":"private prompt"}
{"type":"rename","title":"Named branch","explicit":true}
`, "Named branch"},
		{"malformed rename", `{"type":"meta","meta":{"id":"test","title":"Named session"}}
{"type":"rename","title":false}
`, "Named session"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			if err := os.WriteFile(path, []byte(tt.rows), 0600); err != nil {
				t.Fatal(err)
			}
			name, err := SessionName(path)
			if err != nil || name != tt.want {
				t.Fatalf("SessionName = %q, %v, want %q", name, err, tt.want)
			}
		})
	}
}

func TestRenameSessionMarksExplicitBranchName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"meta","meta":{"id":"test","parent":"parent"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RenameSession(path, "Named branch"); err != nil {
		t.Fatal(err)
	}
	name, err := SessionName(path)
	if err != nil || name != "Named branch" {
		t.Fatalf("SessionName = %q, %v", name, err)
	}
	if _, err := SessionName(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing session did not return an error")
	}
}
