package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"parallax/internal/ffmpeg"
	"parallax/internal/llm"
	"parallax/internal/projects"
)

type TimelineEnv struct {
	Transaction *projects.TimelineTransaction
	Store       *projects.Store
	ProjectID   string
	Workspace   string
	Bins        ffmpeg.Bins
}

func RegisterTimeline(reg *Registry, env TimelineEnv) {
	reg.Register(llm.NewFunctionTool(
		"get_timeline",
		"Inspect the current staged timeline, including stable item IDs, tracks, frame timing, editable properties, keyframes, and transitions. Call this before editing the timeline.",
		json.RawMessage(`{"type":"object","properties":{"detail":{"type":"string","description":"Optional focus such as titles, audio, or all"}}}`),
	), env.getTimeline)
	reg.Register(llm.NewFunctionTool(
		"place_media",
		"Put a workspace media file on the timeline the same way the editor does. Probes the file, places picture on V1, and adds a linked A1 audio clip when the file has sound. Stills go on V1. Audio-only files go on A1. Prefer this over hand-building add_item for imported files.",
		json.RawMessage(`{
			"type":"object",
			"properties":{
				"path":{"type":"string","description":"Workspace-relative media path such as media/talk.mp4"},
				"start_frame":{"type":"integer","description":"Timeline start frame. Omit to append after the last clip, or 0 when the timeline is empty."},
				"at":{"type":"string","description":"Optional placement: end (default), start, or playhead"}
			},
			"required":["path"]
		}`),
	), env.placeMedia)
	reg.Register(llm.NewFunctionTool(
		"edit_timeline",
		"Apply one atomic batch of validated non-destructive timeline operations. operations_json must be a JSON array. Each object needs type and the fields for that operation. Types: place_media uses path; add_item/update_item use item; remove_items uses ids; move_item uses id/start_frame/track; trim_item uses id and timing fields; split_item uses id/frame; transition operations use transition or id. For imported video/audio/images prefer place_media. Use this for titles, caption size (C1 title.font_size and transform.scale), trims, moves, grades, keyframes, and transitions.",
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

func (e TimelineEnv) placeMedia(ctx context.Context, raw json.RawMessage) Result {
	if e.Transaction == nil {
		return Result{OK: false, Error: "timeline transaction is unavailable"}
	}
	var body struct {
		Path       string `json:"path"`
		StartFrame *int   `json:"start_frame"`
		At         string `json:"at"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	ops, err := e.placementOps(ctx, e.Transaction.Get(), projects.TimelineOperation{
		Type:       "place_media",
		Path:       body.Path,
		StartFrame: body.StartFrame,
		At:         body.At,
	})
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	return e.applyOps(ops, "Media is on the timeline. Video is on V1; sound is a linked A1 clip when the file has audio.")
}

func (e TimelineEnv) editTimeline(ctx context.Context, raw json.RawMessage) Result {
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
	ops, err := e.expandMediaOps(ctx, e.Transaction.Get(), body.Operations)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	return e.applyOps(ops, "The timeline change is staged and will commit with the current Director request.")
}

func (e TimelineEnv) applyOps(ops []projects.TimelineOperation, note string) Result {
	result, err := e.Transaction.Apply(ops)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if id, frame := focusTarget(result); id != "" {
		e.Transaction.Focus(id, frame)
		result.Timeline = e.Transaction.Get()
	}
	return Result{OK: true, Output: map[string]any{
		"timeline": result.Timeline, "created_ids": result.CreatedIDs, "removed_ids": result.RemovedIDs,
		"staged": true, "note": note,
	}}
}

func (e TimelineEnv) expandMediaOps(ctx context.Context, doc projects.Timeline, ops []projects.TimelineOperation) ([]projects.TimelineOperation, error) {
	out := make([]projects.TimelineOperation, 0, len(ops)+4)
	for _, op := range ops {
		switch strings.TrimSpace(op.Type) {
		case "place_media":
			placed, err := e.placementOps(ctx, doc, op)
			if err != nil {
				return nil, err
			}
			out = append(out, placed...)
			for _, next := range placed {
				if next.Item != nil {
					doc.Clips = append(doc.Clips, *next.Item)
				}
			}
		case "add_item":
			expanded, extra := e.completeAddItem(ctx, doc, op, ops)
			out = append(out, expanded)
			if extra != nil {
				out = append(out, *extra)
				doc.Clips = append(doc.Clips, *extra.Item)
			}
			if expanded.Item != nil {
				doc.Clips = append(doc.Clips, *expanded.Item)
			}
		default:
			out = append(out, op)
		}
	}
	return out, nil
}

func (e TimelineEnv) placementOps(ctx context.Context, doc projects.Timeline, op projects.TimelineOperation) ([]projects.TimelineOperation, error) {
	path := strings.TrimSpace(op.Path)
	if path == "" && op.Item != nil {
		path = op.Item.MediaPath
	}
	if path == "" {
		return nil, jsonError("place_media requires path")
	}
	rel, info, err := e.probePlacement(ctx, path)
	if err != nil {
		return nil, err
	}
	layout := layoutFromProbe(rel, doc, info, op.StartFrame, op.At)
	clips := projects.PlaceMediaClips(layout)
	if len(clips) == 0 {
		return nil, jsonError("could not place " + rel)
	}
	ops := make([]projects.TimelineOperation, 0, len(clips))
	for i := range clips {
		item := clips[i]
		ops = append(ops, projects.TimelineOperation{Type: "add_item", Item: &item})
	}
	return ops, nil
}

func (e TimelineEnv) completeAddItem(ctx context.Context, doc projects.Timeline, op projects.TimelineOperation, batch []projects.TimelineOperation) (projects.TimelineOperation, *projects.TimelineOperation) {
	if op.Item == nil || strings.TrimSpace(op.Item.MediaPath) == "" || op.Item.Kind == "title" || op.Item.Kind == "caption" || op.Item.Track == "V2" || op.Item.Track == "C1" {
		return op, nil
	}
	rel, info, err := e.probePlacement(ctx, op.Item.MediaPath)
	if err != nil {
		return op, nil
	}
	item := *op.Item
	item.MediaPath = rel
	fps := doc.FPS
	if fps < 1 {
		fps = 24
	}
	sourceFrames := sourceFramesFromProbe(rel, info, fps)
	if item.DurationFrames < 1 || projects.LooksLikeSecondsAsFrames(item.DurationFrames, info.Duration, fps) {
		item.DurationFrames = sourceFrames
	}
	if item.SourceDurationFrames < 1 {
		item.SourceDurationFrames = sourceFrames
	}
	if item.Name == "" {
		item.Name = strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
	}
	isImage := projects.KindForExt(filepath.Ext(rel)) == "image"
	hasPicture := info.HasVideo || isImage || projects.KindForExt(filepath.Ext(rel)) == "video"
	hasAudio := info.HasAudio && !isImage
	if item.Track == "" {
		if hasPicture {
			item.Track = "V1"
		} else {
			item.Track = "A1"
		}
	}
	if item.Kind == "" {
		if item.Track == "A1" || item.Track == "A2" {
			item.Kind = "audio"
		} else {
			item.Kind = "video"
		}
	}
	if item.MediaType == "" {
		switch {
		case isImage:
			item.MediaType = "image"
		case item.Kind == "audio":
			item.MediaType = "audio"
		default:
			item.MediaType = "video"
		}
	}
	op.Item = &item
	if item.Kind != "video" || item.Track != "V1" || !hasAudio || alreadyHasLinkedAudio(doc, batch, item) {
		return op, nil
	}
	layout := layoutFromProbe(rel, doc, info, &item.StartFrame, "")
	layout.HasPicture = false
	layout.HasAudio = true
	layout.StartFrame = item.StartFrame
	layout.DurationFrames = item.DurationFrames
	layout.SourceDurationFrames = item.SourceDurationFrames
	layout.Name = item.Name
	audioClips := projects.PlaceMediaClips(layout)
	if len(audioClips) == 0 {
		return op, nil
	}
	audio := audioClips[0]
	if item.LinkID != "" {
		audio.LinkID = item.LinkID
	} else {
		item.LinkID = audio.LinkID
	}
	op.Item = &item
	return op, &projects.TimelineOperation{Type: "add_item", Item: &audio}
}

func focusTarget(result projects.OperationResult) (string, int) {
	created := map[string]bool{}
	for _, id := range result.CreatedIDs {
		created[id] = true
	}
	id := ""
	frame := -1
	for _, clip := range result.Timeline.Clips {
		if !created[clip.ID] {
			continue
		}
		if id == "" || clip.Kind == "video" {
			id = clip.ID
			frame = clip.StartFrame
		}
	}
	return id, frame
}

func alreadyHasLinkedAudio(doc projects.Timeline, batch []projects.TimelineOperation, item projects.TimelineClip) bool {
	if item.LinkID != "" {
		for _, clip := range doc.Clips {
			if clip.LinkID == item.LinkID && clip.Kind == "audio" {
				return true
			}
		}
	}
	for _, op := range batch {
		if strings.TrimSpace(op.Type) != "add_item" || op.Item == nil {
			continue
		}
		other := *op.Item
		if other.Kind != "audio" && other.Track != "A1" && other.Track != "A2" {
			continue
		}
		if item.LinkID != "" && other.LinkID == item.LinkID {
			return true
		}
		if strings.TrimSpace(other.MediaPath) != "" && other.MediaPath == item.MediaPath && other.StartFrame == item.StartFrame {
			return true
		}
	}
	return false
}

func (e TimelineEnv) probePlacement(ctx context.Context, path string) (string, ffmpeg.MediaProbe, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", ffmpeg.MediaProbe{}, jsonError("media path is required")
	}
	if e.Workspace == "" {
		rel, err := sanitizeRel(path)
		return rel, ffmpeg.MediaProbe{}, err
	}
	abs, err := ffmpeg.ResolveInWorkspace(e.Workspace, path)
	if err != nil {
		return "", ffmpeg.MediaProbe{}, err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", ffmpeg.MediaProbe{}, jsonError("media file not found: " + path)
	}
	rel, err := filepath.Rel(e.Workspace, abs)
	if err != nil {
		return "", ffmpeg.MediaProbe{}, err
	}
	rel = filepath.ToSlash(rel)
	info, err := ffmpeg.ProbeMedia(ctx, e.Bins, e.Workspace, rel)
	if err != nil {
		return rel, ffmpeg.MediaProbe{}, jsonError("could not probe " + rel + ": " + err.Error())
	}
	return rel, info, nil
}

func layoutFromProbe(path string, doc projects.Timeline, info ffmpeg.MediaProbe, start *int, at string) projects.MediaLayout {
	fps := doc.FPS
	if fps < 1 {
		fps = 24
	}
	isImage := projects.KindForExt(filepath.Ext(path)) == "image"
	extKind := projects.KindForExt(filepath.Ext(path))
	hasPicture := info.HasVideo || isImage || extKind == "video"
	hasAudio := info.HasAudio && !isImage
	if extKind == "audio" {
		hasPicture = false
		hasAudio = true
	}
	frames := sourceFramesFromProbe(path, info, fps)
	startFrame := projects.TimelineEndFrame(doc)
	switch strings.ToLower(strings.TrimSpace(at)) {
	case "start":
		startFrame = 0
	case "playhead":
		startFrame = doc.PlayheadFrame
	}
	if start != nil && *start >= 0 {
		startFrame = *start
	}
	return projects.MediaLayout{
		Path:                 path,
		StartFrame:           startFrame,
		DurationFrames:       frames,
		SourceDurationFrames: frames,
		HasPicture:           hasPicture,
		HasAudio:             hasAudio,
		IsImage:              isImage,
	}
}

func sourceFramesFromProbe(path string, info ffmpeg.MediaProbe, fps int) int {
	if projects.KindForExt(filepath.Ext(path)) == "image" {
		return projects.SecondsToFrames(5, fps)
	}
	if info.Duration > 0 {
		return projects.SecondsToFrames(info.Duration, fps)
	}
	return projects.SecondsToFrames(5, fps)
}

func sanitizeRel(path string) (string, error) {
	path = filepath.ToSlash(strings.TrimSpace(path))
	if path == "" || strings.HasPrefix(path, "/") || strings.Contains(path, "://") {
		return "", jsonError("media path must be project-relative")
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", jsonError("media path escapes the project")
	}
	return clean, nil
}

func jsonError(message string) error {
	return &toolError{message}
}

type toolError struct{ msg string }

func (e *toolError) Error() string { return e.msg }
