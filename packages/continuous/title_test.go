package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

func TestConversationTitlePersists(t *testing.T) {
	for _, backend := range crashBackends() {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "store")
			open := func() *Runtime {
				s, err := openCrashStore(ctx, backend, path)
				if err != nil {
					t.Fatal(err)
				}
				r, err := New(s)
				if err != nil {
					s.Close()
					t.Fatal(err)
				}
				t.Cleanup(func() { r.Close() })
				return r
			}
			r := open()
			c, err := r.OpenRoot(ctx, "workspace", AgentConfig{})
			if err != nil {
				t.Fatal(err)
			}
			// An unrelated commit does not invalidate the conversation revision.
			if _, err := r.OpenRoot(ctx, "other", AgentConfig{}); err != nil {
				t.Fatal(err)
			}
			renamed, err := r.Rename(ctx, c.ID, c.Revision, "  Prüfung  ")
			if err != nil {
				t.Fatal(err)
			}
			want := c
			want.Title, want.Revision = "Prüfung", renamed.Revision
			if !reflect.DeepEqual(renamed, want) {
				t.Fatalf("rename changed more than metadata: %+v", renamed)
			}
			if _, err := r.Rename(ctx, c.ID, c.Revision, "stale"); !errors.Is(err, storage.ErrConflict) {
				t.Fatalf("stale rename: %v", err)
			}
			commits, err := r.Scan(ctx, renamed.Revision-1, 1)
			if err != nil || len(commits) != 1 || len(commits[0].Operations) != 1 {
				t.Fatalf("rename must be one record: %v", err)
			}
			ops := viewOps(commits[0], c.ID)
			found := false
			for _, op := range ops {
				if op.Doc == ViewMetadata && string(op.Value) == `{"title":"Prüfung"}` {
					found = true
				}
			}
			if !found {
				t.Fatal("rename missing from view watch")
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			r = open()
			got, err := r.OpenRoot(ctx, "workspace", AgentConfig{})
			if err != nil || !reflect.DeepEqual(got, renamed) {
				t.Fatalf("title lost on reopen: %+v %v", got, err)
			}
			list, _, err := r.Conversations(ctx, "", 10)
			if err != nil {
				t.Fatal(err)
			}
			found = false
			for _, item := range list {
				if item.ID == c.ID && item.Title == "Prüfung" {
					found = true
				}
			}
			if !found {
				t.Fatal("title missing from conversation list")
			}
			snapshot, err := r.ConversationSnapshot(ctx, c.ID, 10)
			if err != nil || snapshot.Conversation.Title != "Prüfung" {
				t.Fatalf("snapshot title: %v", err)
			}
			cleared, err := r.Rename(ctx, c.ID, got.Revision, " ")
			if err != nil || cleared.Title != "" {
				t.Fatalf("clear title: %v", err)
			}
			unchanged, err := r.Rename(ctx, c.ID, cleared.Revision, "")
			if err != nil || unchanged.Revision != cleared.Revision {
				t.Fatalf("no-op rename: %v", err)
			}
		})
	}
}

func TestConversationTitleValidation(t *testing.T) {
	ctx := context.Background()
	r := newTestRuntime(t)
	c, err := r.OpenRoot(ctx, "workspace", AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{strings.Repeat("ü", 121), "a\nb", "a\x00b", string([]byte{0xff})} {
		if _, err := r.Rename(ctx, c.ID, c.Revision, title); err == nil {
			t.Fatal("accepted invalid title")
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.Rename(cancelled, c.ID, c.Revision, "title"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled rename: %v", err)
	}
	if _, err := r.Rename(ctx, "missing", 0, "title"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing conversation: %v", err)
	}
	var legacy Conversation
	if err := json.Unmarshal([]byte(`{"id":"legacy","revision":1}`), &legacy); err != nil || legacy.Title != "" {
		t.Fatal("legacy conversation requires a title")
	}
	// Renaming while a prompt is queued must preserve execution and policy.
	if _, err := r.Submit(ctx, c.ID, "test", "", "hello"); err != nil {
		t.Fatal(err)
	}
	before, err := r.ConversationSnapshot(ctx, c.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Rename(ctx, c.ID, before.Conversation.Revision, strings.Repeat("ü", 120)); err != nil {
		t.Fatal(err)
	}
	after, err := r.ConversationSnapshot(ctx, c.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Entries, after.Entries) || !reflect.DeepEqual(before.Queue, after.Queue) || !reflect.DeepEqual(before.Chain, after.Chain) || before.Conversation.ConfigRevision != after.Conversation.ConfigRevision {
		t.Fatal("rename changed execution or policy")
	}
}

func TestHostConversationRename(t *testing.T) {
	server, _, _, _ := newHostServer(t, map[string]Role{"read": RoleRead, "submit": RoleSubmit})
	reader := dialHost(t, server)
	reader.must("hello", map[string]any{"token": "read"})
	writer := dialHost(t, server)
	writer.must("hello", map[string]any{"token": "submit"})
	c := writer.must("conversation.create", map[string]any{"workspace": "title"})
	p := map[string]any{"id": c["id"], "expected_revision": c["revision"], "title": "Shared title"}
	if got := reader.call("conversation.rename", p); got["code"] != "forbidden" {
		t.Fatalf("read role renamed: %v", got)
	}
	renamed := writer.must("conversation.rename", p)
	if renamed["title"] != "Shared title" || renamed["id"] != c["id"] {
		t.Fatalf("rename response: %v", renamed)
	}
	if got := writer.call("conversation.rename", p); got["code"] != "conflict" {
		t.Fatalf("stale revision accepted: %v", got)
	}
	delete(p, "title")
	p["expected_revision"] = renamed["revision"]
	if got := writer.call("conversation.rename", p); got["success"] != false {
		t.Fatal("missing title silently cleared name")
	}
	p["title"] = ""
	if got := writer.must("conversation.rename", p); got["title"] != "" {
		t.Fatal("title not cleared")
	}
}
