package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/patriceckhart/zot/packages/agent/codemode"
	"github.com/patriceckhart/zot/packages/agent/extensions"
	"github.com/patriceckhart/zot/packages/agent/tools"
	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/core"
)

// loadContinuousEngine stages and validates a fresh generation. It never
// reloads an existing manager, so a failed load leaves older engines intact.
func loadContinuousEngine(ctx context.Context, args Args, rt *continuous.Runtime, service func() *continuous.Service) (continuous.Engine, func(), error) {
	r, err := Resolve(args, true)
	if err != nil {
		return nil, nil, err
	}
	mgr, stop, errs := loadNonInteractiveExtensions(ctx, args, &r, "continuous")
	if err := errors.Join(errs...); err != nil {
		stop()
		return nil, nil, fmt.Errorf("load continuous extensions: %w", err)
	}
	engine := newContinuousEngine(r, mgr, rt, service)
	if _, err := engine.Build(ctx, continuous.Conversation{ID: "reload-probe"}); err != nil {
		stop()
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		stop()
		return nil, nil, err
	}
	return engine, stop, nil
}

// newContinuousEngine captures one immutable host configuration and extension
// manager. Every build gets its own registry, since tasks build agents in parallel.
// Inference settings are resolved per conversation, without changing host tools,
// filesystem permissions, project instructions, or extension generations.
func newContinuousEngine(resolved Resolved, mgr *extensions.Manager, rt *continuous.Runtime, service func() *continuous.Service) continuous.Engine {
	return continuous.EngineFunc(func(ctx context.Context, c continuous.Conversation) (*core.Agent, error) {
		inference, err := continuousInference(ctx, resolved, c.Config)
		if err != nil {
			return nil, err
		}
		local := resolved
		local.Provider, local.Model = inference.Provider, inference.Model
		local.Credential, local.AuthMethod, local.AccountID = inference.Credential, inference.AuthMethod, inference.AccountID
		local.BaseURL, local.InsecureTLS = inference.BaseURL, inference.InsecureTLS
		local.MaxOutput, local.credentialOptional = inference.MaxOutput, inference.credentialOptional
		local.ToolRegistry = make(core.Registry, len(resolved.ToolRegistry)+2)
		for name, tool := range resolved.ToolRegistry {
			if tools.IsOpenRouterServerTool(name) && (local.Provider != "openrouter" || isOpenRouterPresetModel(local.Model)) {
				continue
			}
			if original, ok := tool.(*codemode.Tool); ok {
				copy := *original
				copy.Models = newCodemodeModelCaller(local.Provider, local.Credential, local.BaseURL)
				tool = &copy
			}
			local.ToolRegistry[name] = tool
		}
		if resolved.openRouterServerTools && local.Provider == "openrouter" && !isOpenRouterPresetModel(local.Model) {
			local.ToolRegistry = withOpenRouterServerTools(local.ToolRegistry)
		}
		if hasOpenRouterServerTools(local.ToolRegistry) != hasOpenRouterServerTools(resolved.ToolRegistry) {
			local.SystemPrompt = BuildSystemPrompt(SystemPromptOpts{
				CWD: local.CWD, Tools: toolSummariesFromRegistry(local.ToolRegistry, local.toolDescriptions),
				Custom: local.systemCustom, CustomSet: local.systemCustomSet, Append: local.systemAppend,
				ZotDocsDir: filepath.Join(ZotHome(), "docs"),
			})
		}
		ag := local.NewAgent()
		wireNonInteractiveAgentExtHooks(ctx, ag, mgr)
		ag.Tools["subagent"] = &continuous.SubagentTool{Runtime: rt, Service: service()}
		ag.Tools["handoff"] = continuous.HandoffTool{}
		return ag, nil
	})
}
