package continuous

import (
	"encoding/json"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
)

// NestedRecovery preserves the ownership hierarchy in recovery previews.
// Children are conditional on their parent's fresh authorization and replay.
type NestedRecovery struct {
	Task       string                `json:"task"`
	ParentTask string                `json:"parent_task"`
	CallID     string                `json:"call_id"`
	Name       string                `json:"name"`
	Depth      int                   `json:"depth"`
	State      string                `json:"state"`
	Effect     string                `json:"effect,omitempty"`
	Replay     core.ToolReplayPolicy `json:"replay,omitempty"`
	Action     string                `json:"action"`
	Children   []NestedRecovery      `json:"children,omitempty"`
}

func nestedRecovery(snap storage.Snapshot, parent Task, canReplay bool) ([]NestedRecovery, bool) {
	return nestedRecoveryAt(snap, parent, canReplay, 0)
}

func nestedRecoveryAt(snap storage.Snapshot, parent Task, canReplay bool, depth int) ([]NestedRecovery, bool) {
	if depth > 3 {
		return nil, false
	}
	var children []Task
	err := pageAll(snap, "task-owner/"+parent.ID+"/", func(row storage.Record) error {
		var id string
		if json.Unmarshal(row.Value, &id) != nil {
			return storage.ErrCorrupt
		}
		child, ok, err := read[Task](snap, taskKey(id))
		if err != nil {
			return err
		}
		if !ok {
			return storage.ErrCorrupt
		}
		if child.Kind == TaskKindTool {
			children = append(children, child)
		}
		return nil
	})
	if err != nil {
		return nil, false
	}
	sortTasks(children)
	automatic := true
	var out []NestedRecovery
	for _, child := range children {
		in, cp, err := decodeTool(child)
		if err != nil {
			return nil, false
		}
		if in.ParentTask == "" {
			continue
		}
		n := NestedRecovery{Task: child.ID, ParentTask: parent.ID, CallID: in.CallID, Name: in.Name, Depth: in.Depth, State: child.State, Effect: child.Effect, Replay: cp.Replay, Action: "continue"}
		replay := canReplay
		switch {
		case child.State == "terminal":
			n.Action = "reuse"
		case child.AbortRequested || !canReplay:
			n.Action = "abort"
			replay = false
		case child.WaitApproval != "":
			n.Action = "approve"
		case child.Effect == effectStarted:
			switch cp.Replay {
			case core.ReplaySafe, core.ReplayIdempotent:
				n.Action = "replay"
			case core.ReplayReconcile:
				n.Action = "reconcile"
			default:
				n.Action = "report"
				replay, automatic = false, false
			}
		}
		var safe bool
		n.Children, safe = nestedRecoveryAt(snap, child, replay, depth+1)
		automatic = automatic && safe
		out = append(out, n)
	}
	return out, automatic
}
