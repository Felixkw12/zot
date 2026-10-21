package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"weak"

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
	// Keep only host tools in the generation snapshot. Extension and MCP
	// tools are collected from the current catalog for each build/refresh.
	hostTools := core.Registry{}
	for name, tool := range resolved.ToolRegistry {
		if mgr == nil || !mgr.HasTool(name) {
			hostTools[name] = tool
		}
	}
	resolved.ToolRegistry = hostTools
	type liveAgent struct {
		agent    weak.Pointer[core.Agent]
		builtins core.Registry
	}
	var refreshMu sync.Mutex
	var agents []liveAgent
	if mgr != nil {
		mgr.SetToolCaller(extensionToolCaller(mgr, nil))
		mgr.SetOnReload(func() {
			refreshMu.Lock()
			defer refreshMu.Unlock()
			live := agents[:0]
			for _, entry := range agents {
				if ag := entry.agent.Value(); ag != nil {
					ag.SetTools(extensionToolRegistry(entry.builtins, mgr))
					live = append(live, entry)
				}
			}
			// Do not retain completed agents or their conversation tools.
			clear(agents[len(live):])
			agents = live
		})
	}
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
		wireNonInteractiveAgentExtLifecycle(ctx, ag, mgr)
		ag.Tools["subagent"] = &continuous.SubagentTool{Runtime: rt, Service: service()}
		ag.Tools["handoff"] = continuous.HandoffTool{}
		if mgr != nil {
			// Serialize registration with notifications so a build cannot
			// miss a change between taking its snapshot and subscribing.
			refreshMu.Lock()
			// Also prune on builds when the server catalog never changes.
			agents = slices.DeleteFunc(agents, func(entry liveAgent) bool {
				return entry.agent.Value() == nil
			})
			builtins := ag.ToolRegistry()
			ag.SetTools(extensionToolRegistry(builtins, mgr))
			agents = append(agents, liveAgent{agent: weak.Make(ag), builtins: builtins})
			refreshMu.Unlock()
		}
		return ag, nil
	})
}
