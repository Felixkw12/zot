package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

type toolTaskContextKey struct{}
type nestedBindingKey struct{}
type nestedBinding struct {
	callID  string
	guard   *core.Agent
	execute func(context.Context, provider.ToolCallBlock) core.ToolResult
}

func blockedNested(reason string) core.ToolResult {
	return core.ToolResult{IsError: true, Status: "blocked", Content: []provider.Content{provider.TextBlock{Text: reason}}}
}

// The parent supplies the live operation again on replay. No executable
// closure is stored, and the scheduler cannot independently replay a child.
func (x *NativeExecutor) wireNestedExecution(agent *core.Agent, conversationID string) {
	before, beforeCtx := agent.BeforeToolExecute, agent.BeforeToolExecuteContext
	agent.NestedToolExecutor = func(ctx context.Context, parent, call provider.ToolCallBlock, operation core.Tool, execute func(context.Context, provider.ToolCallBlock) core.ToolResult) core.ToolResult {
		identity, ok := ToolCallFromContext(ctx)
		tc, attached := ctx.Value(toolTaskContextKey{}).(TaskContext)
		if !ok || !attached || tc.inv == nil || identity.CallID != parent.ID || identity.ConversationID != conversationID {
			return blockedNested("nested tool call refused: no resolvable parent task")
		}
		in, _, err := decodeTool(tc.inv.current())
		if err != nil || in.RunID != identity.RunID || in.CallID != parent.ID || in.Depth >= 3 {
			return blockedNested("nested tool call refused: invalid parent task or depth")
		}
		key := strings.TrimPrefix(call.ID, parent.ID+"/n/")
		if key == call.ID || key == "" {
			return blockedNested("nested tool call refused: invalid key")
		}
		args, err := normalizedNestedArgs(call.Arguments)
		if err != nil {
			return blockedNested("nested tool call refused: invalid arguments")
		}
		childID := hashedKey("", tc.Task.ID, key)
		err = tc.Commit(ctx, func(tx *TaskTx) error {
			if tx.Task().AbortRequested || tx.Task().State == "terminal" {
				return ErrTaskAborting
			}
			child, exists, err := read[Task](tx.View(), taskKey(childID))
			if err != nil {
				return err
			}
			if exists {
				other, _, err := decodeTool(child)
				if err != nil || child.Owner != tc.Task.ID || other.ParentTask != tc.Task.ID || other.NestedKey != key || other.Name != call.Name || !sameJSON(other.Args, args) {
					return fmt.Errorf("nested key %q was reused with a different operation", key)
				}
				return nil
			}
			origin := firstNonEmpty(core.NestedToolOrigin(ctx), "runtime")
			_, err = tx.CreateChild(TaskSpec{ID: childID, Kind: TaskKindTool, Input: toolInput{RunID: in.RunID, CallID: call.ID, Name: call.Name, Args: args, ParentTask: tc.Task.ID, NestedKey: key, Depth: in.Depth + 1, Origin: origin, Turn: in.Turn}})
			return err
		})
		if err != nil {
			return blockedNested("nested tool call refused: " + err.Error())
		}
		tools := core.Registry{}
		for name, tool := range agent.Tools {
			tools[name] = tool
		}
		if operation != nil {
			tools[operation.Name()] = operation
		}
		guard := &core.Agent{Tools: tools, BeforeToolExecute: before, BeforeToolExecuteContext: beforeCtx, Model: agent.Model, Client: agent.Client}
		binding := &nestedBinding{callID: call.ID, guard: guard, execute: execute}
		ctx = context.WithValue(ctx, nestedBindingKey{}, binding)
		return x.driveNested(ctx, tc, childID)
	}
}

func normalizedNestedArgs(args json.RawMessage) (json.RawMessage, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(args)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if !json.Valid(args) {
		return nil, fmt.Errorf("invalid JSON")
	}
	return json.Marshal(value)
}

// Drive child phases inline under the store-wide invocation claim. A parked
// approval or dependency releases the parent's worker slot, but keeps the
// invocation fenced and joined. Committed state is always the source of truth.
func (x *NativeExecutor) driveNested(ctx context.Context, parent TaskContext, childID string) core.ToolResult {
	s := parent.inv.s
	for {
		snap, err := x.r.store.Snapshot(ctx)
		if err != nil {
			return blockedNested("nested tool call interrupted: " + err.Error())
		}
		p, ok, err := read[Task](snap, taskKey(parent.Task.ID))
		if err != nil || !ok || p.State == "terminal" || p.AbortRequested || p.Invocation != parent.inv.current().Invocation {
			return blockedNested("nested tool call refused: parent is no longer live")
		}
		child, ok, err := read[Task](snap, taskKey(childID))
		if err != nil || !ok {
			return blockedNested("nested tool call refused: child unavailable")
		}
		if child.State == "terminal" {
			var outcome toolOutcome
			if json.Unmarshal(child.Result, &outcome) != nil || outcome.Nested == nil {
				return blockedNested("nested tool task failed: " + child.Error)
			}
			result, err := outcome.Nested.result()
			if err != nil {
				return blockedNested("nested tool result unavailable: " + err.Error())
			}
			return result
		}
		ready := true
		if child.RetryAt != nil && time.Now().Before(*child.RetryAt) {
			ready = false
		}
		if child.State == "waiting" {
			switch {
			case child.WaitApproval != "":
				a, found, err := read[Approval](snap, approvalKey(child.WaitApproval))
				if err != nil {
					return blockedNested(err.Error())
				}
				ready = !found || a.State != approvalPending
			case child.WakeAt != nil:
				ready = !time.Now().Before(*child.WakeAt)
			default:
				ready, _, err = dependenciesReady(snap, child)
				if err != nil {
					return blockedNested(err.Error())
				}
			}
		}
		if ready {
			if _, claimed := x.r.claims.LoadOrStore(childID, s); !claimed {
				err := s.runOnce(ctx, child)
				x.r.claims.Delete(childID)
				if err != nil && !errors.Is(err, storage.ErrConflict) {
					return blockedNested("nested execution interrupted: " + err.Error())
				}
				continue
			}
		}
		// Timers require a bounded wake as well as commit notification.
		waitCtx := ctx
		cancel := func() {}
		deadline := child.WakeAt
		if child.RetryAt != nil {
			deadline = child.RetryAt
		}
		if deadline != nil {
			waitCtx, cancel = context.WithDeadline(ctx, *deadline)
		}
		resume := parent.suspend()
		err = x.r.store.Wait(waitCtx, snap.Revision())
		resume()
		cancel()
		if err != nil && ctx.Err() != nil {
			return blockedNested("nested execution interrupted: " + ctx.Err().Error())
		}
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return blockedNested(err.Error())
		}
	}
}

func (x *NativeExecutor) awaitNestedSettlement(ctx context.Context, tc TaskContext) error {
	resume := tc.suspend()
	defer resume()
	for {
		snap, err := x.r.store.Snapshot(ctx)
		if err != nil {
			return err
		}
		settled, err := childrenSettled(snap, tc.Task.ID)
		if err != nil || settled {
			return err
		}
		if err := x.r.store.Wait(ctx, snap.Revision()); err != nil {
			return err
		}
	}
}

func abortNestedChildren(tx *TaskTx) error {
	return pageAll(tx.View(), "task-owner/"+tx.Task().ID+"/", func(row storage.Record) error {
		var id string
		if err := json.Unmarshal(row.Value, &id); err != nil {
			return err
		}
		child, ok, err := read[Task](tx.View(), taskKey(id))
		if err != nil {
			return err
		}
		if ok && child.Kind == TaskKindTool && child.State != "terminal" {
			in, _, err := decodeTool(child)
			if err != nil {
				return err
			}
			if in.ParentTask == tx.Task().ID {
				marks, err := abortMarks(tx.View(), child)
				if err != nil {
					return err
				}
				tx.Ops(marks...)
			}
		}
		return nil
	})
}

// nestedApprovalReleasable reports whether the conversation's progress is
// blocked only by pending approvals of nested calls whose ancestors all
// replay safely. Releasing such parents loses nothing: their replay finds the
// committed child by key and parks on the same approval again.
func nestedApprovalReleasable(snap storage.Snapshot, conversationID string) bool {
	pending, err := pendingApprovals(snap, conversationID)
	if err != nil || len(pending) == 0 {
		return false
	}
	waiting := map[string]bool{}
	for _, a := range pending {
		waiting[a.ID] = true
	}
	found := false
	err = pageAll(snap, "task-conversation/"+conversationID+"/", func(row storage.Record) error {
		var id string
		if json.Unmarshal(row.Value, &id) != nil {
			return storage.ErrCorrupt
		}
		t, ok, err := read[Task](snap, taskKey(id))
		if err != nil || !ok {
			return storage.ErrCorrupt
		}
		if t.Kind != TaskKindTool || t.State == "terminal" || !waiting[t.WaitApproval] {
			return nil
		}
		in, _, err := decodeTool(t)
		if err != nil || in.ParentTask == "" {
			return errNotReleasable
		}
		for parentID := in.ParentTask; parentID != ""; {
			parent, ok, err := read[Task](snap, taskKey(parentID))
			if err != nil || !ok || parent.State == "terminal" || parent.AbortRequested {
				return errNotReleasable
			}
			pin, cp, err := decodeTool(parent)
			if err != nil || parent.Effect != effectStarted || (cp.Replay != core.ReplaySafe && cp.Replay != core.ReplayIdempotent) {
				return errNotReleasable
			}
			parentID = pin.ParentTask
		}
		found = true
		return nil
	})
	return err == nil && found
}

var errNotReleasable = errors.New("nested approval wait is not releasable")
