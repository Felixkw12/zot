package agent

import (
	"os"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
)

func TestNoSessionConversationIdentity(t *testing.T) {
	isolateSessionEnvironment(t)
	home := t.TempDir()
	t.Setenv("ZOT_HOME", home)
	r := Resolved{Provider: "opencode-go", Model: "kimi-k3"}
	ag := core.NewAgent(nil, r.Model, "", nil)
	var id string
	for range 2 {
		session, err := openOrCreateSession(Args{NoSess: true}, r, ag, "test")
		if err != nil || session != nil {
			t.Fatalf("non-persisted session = %v, %v", session, err)
		}
		if ag.SessionID == "" {
			t.Fatal("non-persisted conversation has no ID")
		}
		if id == "" {
			id = ag.SessionID
		} else if ag.SessionID != id {
			t.Fatal("reopening a non-persisted conversation changed its ID")
		}
		bindAgentSession(ag, nil)
		if ag.SessionID != id {
			t.Fatal("conversation ID changed on rebinding")
		}
	}
	other := core.NewAgent(nil, r.Model, "", nil)
	bindAgentSession(other, nil)
	if other.SessionID == "" || other.SessionID == ag.SessionID {
		t.Fatal("independent conversation did not get a unique ID")
	}
	assertSessionEnvironment(t, r, nil)
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("non-persisted conversation wrote files")
	}
}

func TestReplacementAgentKeepsConversationIdentity(t *testing.T) {
	current := core.NewAgent(nil, "old-model", "", nil)
	bindAgentSession(current, nil)
	replacement := core.NewAgent(nil, "new-model", "", nil)
	bindReplacementAgentSession(replacement, current, nil)
	if replacement.SessionID == "" || replacement.SessionID != current.SessionID {
		t.Fatal("model switch changed the transient conversation ID")
	}
	bindReplacementAgentSession(replacement, current, &core.Session{ID: "persisted"})
	if replacement.SessionID != "persisted" {
		t.Fatal("persisted session ID did not override transient ID")
	}
	bindReplacementAgentSession(nil, current, nil)
	bindAgentSession(nil, nil)
}
