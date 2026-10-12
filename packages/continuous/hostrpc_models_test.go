package continuous

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestModelListUsesHostProviderAndReadRole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := newTestRuntime(t)
	engine := echoEngine(&scriptedClient{}, core.NewRegistry())
	host, err := NewHost(r, engine, HostOptions{})
	if err != nil {
		t.Fatal(err)
	}
	token := "read-only-token-0123456789"
	server := &HostServer{Host: host, Engine: engine, Tokens: map[string]Role{token: RoleRead}, DefaultConfig: AgentConfig{Provider: "anthropic"}}
	a, b := net.Pipe()
	defer a.Close()
	go server.ServeConn(ctx, b)
	client, err := NewClient(ctx, a, token)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Provider string      `json:"provider"`
		Models   []HostModel `json:"models"`
	}
	if err := client.CallInto(ctx, "model.list", map[string]any{}, &got); err != nil {
		t.Fatal(err)
	}
	want := provider.ModelsForProvider("anthropic")
	if got.Provider != "anthropic" || len(got.Models) != len(want) || len(want) == 0 {
		t.Fatalf("got provider %q with %d models, want %d", got.Provider, len(got.Models), len(want))
	}
	if got.Models[0].ID != want[0].ID || got.Models[0].ContextWindow != want[0].ContextWindow {
		t.Fatalf("first model %+v, want %s", got.Models[0], want[0].ID)
	}
	if err := client.CallInto(ctx, "model.list", map[string]any{"provider": "no-such-provider"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Models == nil || len(got.Models) != 0 {
		t.Fatalf("unknown provider returned %+v", got.Models)
	}
	// Older SDK hosts ignore the additive available flag and keep defaults.
	if err := client.CallInto(ctx, "model.list", map[string]any{"available": true}, &got); err != nil || got.Provider != "anthropic" {
		t.Fatalf("legacy catalog fallback: %v provider=%s", err, got.Provider)
	}
}

func TestAvailableModelListAndConfigurationValidation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := newTestRuntime(t)
	engine := echoEngine(&scriptedClient{}, core.NewRegistry())
	host, err := NewHost(r, engine, HostOptions{})
	if err != nil {
		t.Fatal(err)
	}
	server := &HostServer{Host: host, Engine: engine,
		AvailableModels: func(context.Context) ([]HostModel, error) {
			return []HostModel{{Provider: "first", ID: "shared"}, {Provider: "second", ID: "shared"}}, nil
		},
		ValidateConfig: func(_ context.Context, config AgentConfig) error {
			if config.Provider == "invalid" {
				return ErrUnsupported
			}
			return nil
		},
	}
	a, b := net.Pipe()
	defer a.Close()
	go server.ServeConn(ctx, b)
	client, err := NewClient(ctx, a, "")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Models []HostModel `json:"models"`
	}
	if err := client.CallInto(ctx, "model.list", map[string]any{"available": true}, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 2 || got.Models[1].Provider != "second" {
		t.Fatalf("available model identities lost: %+v", got.Models)
	}
	c, err := r.OpenRoot(ctx, "switch", AgentConfig{Provider: "first", Model: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	params := map[string]any{"id": c.ID, "expected_revision": c.Revision, "config": AgentConfig{Provider: "invalid", Model: "shared"}}
	if err := client.CallInto(ctx, "conversation.configure", params, nil); err == nil {
		t.Fatal("invalid configuration accepted")
	}
	unchanged, _ := r.OpenRoot(ctx, "switch", AgentConfig{})
	if unchanged.Config.Provider != "first" || unchanged.Revision != c.Revision {
		t.Fatal("failed validation mutated configuration")
	}
	params["config"] = AgentConfig{Provider: "second", Model: "shared"}
	var changed Conversation
	if err := client.CallInto(ctx, "conversation.configure", params, &changed); err != nil {
		t.Fatal(err)
	}
	if changed.ID != c.ID || changed.Config.Provider != "second" {
		t.Fatal("provider switch did not preserve the conversation")
	}
}
