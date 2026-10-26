package agent

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/continuous/storage/memory"
	"github.com/patriceckhart/zot/packages/core"
)

func TestAttachedTUIOpensExplicitConversation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rt, err := continuous.New(memory.Open())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	engine := continuous.EngineFunc(func(context.Context, continuous.Conversation) (*core.Agent, error) {
		return core.NewAgent(nil, "host-model", "", core.NewRegistry()), nil
	})
	host, err := continuous.NewHost(rt, engine, continuous.HostOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &continuous.HostServer{Host: host, Engine: engine}
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx, ln) }()
	defer func() { cancel(); <-served }()

	root, err := rt.OpenRoot(ctx, "main", continuous.AgentConfig{Provider: "anthropic", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	// An owned subagent conversation: it has no workspace key, so only an
	// explicit ID can attach to it.
	fork, err := rt.CreateOwnedConversation(ctx, root.ID, "task-1", "child", nil)
	if err != nil {
		t.Fatal(err)
	}
	before, _, err := rt.Conversations(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}

	args := Args{Continuous: ln.Addr().String(), ContinuousConversation: fork.ID, Provider: "openai", Model: "other"}
	session, _, _, closeAttached, err := attachContinuous(ctx, args, Resolved{Model: "other"}, core.NewAgent(nil, "", "", core.NewRegistry()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if session == nil {
		t.Fatal("no attached session")
	}
	closeAttached()
	after, _, err := rt.Conversations(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("attaching by ID created conversations: %d -> %d", len(before), len(after))
	}
	current, err := rt.Conversation(ctx, fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Config.Model == "other" || current.Revision != fork.Revision {
		t.Fatalf("attaching by ID changed the conversation: %+v", current)
	}

	args.ContinuousConversation = "missing"
	if _, _, _, _, err := attachContinuous(ctx, args, Resolved{}, core.NewAgent(nil, "", "", core.NewRegistry()), nil); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing conversation error = %v", err)
	}
}

func TestContinuousConversationFlagValidation(t *testing.T) {
	if _, err := ParseArgs([]string{"--continuous-conversation", "c1"}); err == nil {
		t.Fatal("--continuous-conversation accepted without --continuous")
	}
	if _, err := ParseArgs([]string{"--continuous", "/tmp/h.sock", "--continuous-conversation", "c1", "--continuous-workspace", "w"}); err == nil {
		t.Fatal("workspace and conversation accepted together")
	}
	args, err := ParseArgs([]string{"--continuous", "/tmp/h.sock", "--continuous-conversation", "c1"})
	if err != nil || args.ContinuousConversation != "c1" {
		t.Fatalf("args = %+v, %v", args, err)
	}
}
