package tools

import (
	"context"
	"encoding/json"

	"parallax/internal/llm"
	"parallax/internal/projects"
)

type TimelineEnv struct {
	Transaction *projects.TimelineTransaction
	Store       *projects.Store
	ProjectID   string
}

func RegisterTimeline(reg *Registry, env TimelineEnv) {
	reg.Register(llm.NewFunctionTool(
		"get_timeline",
		"Inspect the current staged timeline, including stable item IDs, tracks, frame timing, editable properties, keyframes, and transitions. Call this before editing the timeline.",
		json.RawMessage(`{"type":"object","properties":{"detail":{"type":"string","description":"Optional focus such as titles, audio, or all"}}}`),
	), env.getTimeline)
	reg.Register(llm.NewFunctionTool(
		"edit_timeline",
		"Apply one atomic batch of validated non-destructive timeline operations. operations_json must be a JSON array. Each object needs type and the fields for that operation. Types: add_item/update_item use item; remove_items uses ids; move_item uses id/start_frame/track; trim_item uses id and timing fields; split_item uses id/frame; transition operations use transition or id. Item supports title, transform, playback, audio, grade, and keyframes. Prefer this over rendering edits into source media.",
		json.RawMessage(`{
			"type":"object",
			"properties":{
				"operations_json":{"type":"string","description":"JSON array containing 1-50 timeline operation objects"}
			},"required":["operations_json"]
		}`),
	), env.editTimeline)
	reg.Register(llm.NewFunctionTool("get_project_history", "List persistent project revisions, alternate futures, and checkpoints before undoing or restoring.", json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer","description":"Optional maximum recent revisions to inspect"}}}`)), env.getHistory)
	reg.Register(llm.NewFunctionTool("undo_project_change", "Stage an undo of the current project revision. This must be the first mutation in the request.", json.RawMessage(`{"type":"object","properties":{"confirm":{"type":"boolean","description":"Set true to confirm the undo"}}}`)), env.undo)
	reg.Register(llm.NewFunctionTool("redo_project_change", "Stage a redo. Provide target_revision when multiple alternate futures exist.", json.RawMessage(`{"type":"object","properties":{"target_revision":{"type":"integer"}}}`)), env.redo)
	reg.Register(llm.NewFunctionTool("restore_project_revision", "Stage restoration of a specific persistent project revision.", json.RawMessage(`{"type":"object","properties":{"revision":{"type":"integer"}},"required":["revision"]}`)), env.restore)
	reg.Register(llm.NewFunctionTool("create_project_checkpoint", "Create a named checkpoint at the state committed by this request.", json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)), env.checkpoint)
}

func (e TimelineEnv) getHistory(_ context.Context, _ json.RawMessage) Result {
	if e.Store == nil {
		return Result{OK: false, Error: "project history is unavailable"}
	}
	history, err := e.Store.History(e.ProjectID)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	revisions := make([]map[string]any, 0, len(history.Revisions))
	redoCandidates := history.RedoCandidates
	if redoCandidates == nil {
		redoCandidates = []int{}
	}
	for _, revision := range history.Revisions {
		revisions = append(revisions, map[string]any{"id": revision.ID, "parent_id": revision.ParentID, "actor": revision.Actor, "summary": revision.Summary, "chat_id": revision.ChatID, "created_at": revision.CreatedAt, "children": revision.Children, "checkpoints": revision.Checkpoints})
	}
	return Result{OK: true, Output: map[string]any{"head": history.Head, "can_undo": history.CanUndo, "redo_candidates": redoCandidates, "revisions": revisions}}
}

func (e TimelineEnv) undo(_ context.Context, _ json.RawMessage) Result {
	doc, err := e.Transaction.StageUndo()
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	return Result{OK: true, Output: map[string]any{"timeline": doc, "staged": true}}
}

func (e TimelineEnv) redo(_ context.Context, raw json.RawMessage) Result {
	var body struct {
		Target int `json:"target_revision"`
	}
	body.Target = -1
	if err := json.Unmarshal(raw, &body); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	doc, err := e.Transaction.StageRedo(body.Target)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	return Result{OK: true, Output: map[string]any{"timeline": doc, "staged": true}}
}

func (e TimelineEnv) restore(_ context.Context, raw json.RawMessage) Result {
	var body struct {
		Revision int `json:"revision"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	doc, err := e.Transaction.StageRestore(body.Revision)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	return Result{OK: true, Output: map[string]any{"timeline": doc, "staged": true}}
}

func (e TimelineEnv) checkpoint(_ context.Context, raw json.RawMessage) Result {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if err := e.Transaction.StageCheckpoint(body.Name); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	return Result{OK: true, Output: map[string]any{"name": body.Name, "staged": true}}
}

func (e TimelineEnv) getTimeline(_ context.Context, _ json.RawMessage) Result {
	if e.Transaction == nil {
		return Result{OK: false, Error: "timeline transaction is unavailable"}
	}
	return Result{OK: true, Output: e.Transaction.Get()}
}

func (e TimelineEnv) editTimeline(_ context.Context, raw json.RawMessage) Result {
	if e.Transaction == nil {
		return Result{OK: false, Error: "timeline transaction is unavailable"}
	}
	var body struct {
		Operations     []projects.TimelineOperation `json:"operations"`
		OperationsJSON string                       `json:"operations_json"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if len(body.Operations) == 0 && body.OperationsJSON != "" {
		if err := json.Unmarshal([]byte(body.OperationsJSON), &body.Operations); err != nil {
			return Result{OK: false, Error: "operations_json: " + err.Error()}
		}
	}
	result, err := e.Transaction.Apply(body.Operations)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	return Result{OK: true, Output: map[string]any{
		"timeline": result.Timeline, "created_ids": result.CreatedIDs, "removed_ids": result.RemovedIDs,
		"staged": true, "note": "The timeline change is staged and will commit with the current Director request.",
	}}
}
