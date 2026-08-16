package projects

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

type TimelineOperation struct {
	Type           string              `json:"type"`
	Item           *TimelineClip       `json:"item,omitempty"`
	Transition     *TimelineTransition `json:"transition,omitempty"`
	ID             string              `json:"id,omitempty"`
	IDs            []string            `json:"ids,omitempty"`
	StartFrame     *int                `json:"start_frame,omitempty"`
	DurationFrames *int                `json:"duration_frames,omitempty"`
	SourceInFrame  *int                `json:"source_in_frame,omitempty"`
	Track          string              `json:"track,omitempty"`
	Frame          int                 `json:"frame,omitempty"`
	Path           string              `json:"path,omitempty"`
	At             string              `json:"at,omitempty"`
}

type OperationResult struct {
	Timeline   Timeline `json:"timeline"`
	CreatedIDs []string `json:"created_ids,omitempty"`
	RemovedIDs []string `json:"removed_ids,omitempty"`
}

func ApplyTimelineOperations(doc Timeline, operations []TimelineOperation) (OperationResult, error) {
	if len(operations) == 0 {
		return OperationResult{}, errors.New("at least one operation is required")
	}
	if len(operations) > 50 {
		return OperationResult{}, errors.New("a transaction can contain at most 50 operations")
	}
	result := OperationResult{Timeline: doc}
	for _, op := range operations {
		var err error
		switch strings.TrimSpace(op.Type) {
		case "add_item":
			if op.Item == nil {
				err = errors.New("add_item requires item")
				break
			}
			item := *op.Item
			if item.ID == "" {
				item.ID = newTimelineID("clip")
			}
			result.Timeline.Clips = append(result.Timeline.Clips, item)
			result.CreatedIDs = append(result.CreatedIDs, item.ID)
		case "update_item":
			if op.Item == nil {
				err = errors.New("update_item requires item")
				break
			}
			id := op.ID
			if id == "" {
				id = op.Item.ID
			}
			index := timelineClipIndex(result.Timeline.Clips, id)
			if index < 0 {
				err = fmt.Errorf("timeline item %q not found", id)
				break
			}
			item := *op.Item
			item.ID = id
			result.Timeline.Clips[index] = item
		case "remove_items":
			ids := append([]string(nil), op.IDs...)
			if op.ID != "" {
				ids = append(ids, op.ID)
			}
			if len(ids) == 0 {
				err = errors.New("remove_items requires ids")
				break
			}
			remove := map[string]bool{}
			for _, id := range ids {
				if timelineClipIndex(result.Timeline.Clips, id) < 0 {
					err = fmt.Errorf("timeline item %q not found", id)
					break
				}
				remove[id] = true
			}
			if err != nil {
				break
			}
			next := result.Timeline.Clips[:0]
			for _, clip := range result.Timeline.Clips {
				if !remove[clip.ID] {
					next = append(next, clip)
				}
			}
			result.Timeline.Clips = next
			transitions := result.Timeline.Transitions[:0]
			for _, tr := range result.Timeline.Transitions {
				if !remove[tr.FromID] && !remove[tr.ToID] {
					transitions = append(transitions, tr)
				}
			}
			result.Timeline.Transitions = transitions
			result.RemovedIDs = append(result.RemovedIDs, ids...)
		case "move_item":
			index := timelineClipIndex(result.Timeline.Clips, op.ID)
			if index < 0 || op.StartFrame == nil {
				err = errors.New("move_item requires an existing id and start_frame")
				break
			}
			result.Timeline.Clips[index].StartFrame = *op.StartFrame
			if op.Track != "" {
				result.Timeline.Clips[index].Track = op.Track
			}
		case "trim_item":
			index := timelineClipIndex(result.Timeline.Clips, op.ID)
			if index < 0 {
				err = fmt.Errorf("timeline item %q not found", op.ID)
				break
			}
			if op.StartFrame != nil {
				result.Timeline.Clips[index].StartFrame = *op.StartFrame
			}
			if op.DurationFrames != nil {
				result.Timeline.Clips[index].DurationFrames = *op.DurationFrames
			}
			if op.SourceInFrame != nil {
				result.Timeline.Clips[index].SourceInFrame = *op.SourceInFrame
			}
		case "split_item":
			index := timelineClipIndex(result.Timeline.Clips, op.ID)
			if index < 0 {
				err = fmt.Errorf("timeline item %q not found", op.ID)
				break
			}
			clip := result.Timeline.Clips[index]
			if op.Frame <= clip.StartFrame || op.Frame >= clip.StartFrame+clip.DurationFrames {
				err = errors.New("split frame must be inside the item")
				break
			}
			leftDuration := op.Frame - clip.StartFrame
			right := clip
			right.ID = newTimelineID("clip")
			right.StartFrame = op.Frame
			right.DurationFrames = clip.DurationFrames - leftDuration
			right.SourceInFrame = clip.SourceInFrame + leftDuration
			clip.DurationFrames = leftDuration
			result.Timeline.Clips[index] = clip
			result.Timeline.Clips = append(result.Timeline.Clips, right)
			result.CreatedIDs = append(result.CreatedIDs, right.ID)
		case "add_transition":
			if op.Transition == nil {
				err = errors.New("add_transition requires transition")
				break
			}
			tr := *op.Transition
			if tr.ID == "" {
				tr.ID = newTimelineID("transition")
			}
			result.Timeline.Transitions = append(result.Timeline.Transitions, tr)
			result.CreatedIDs = append(result.CreatedIDs, tr.ID)
		case "update_transition":
			if op.Transition == nil {
				err = errors.New("update_transition requires transition")
				break
			}
			id := op.ID
			if id == "" {
				id = op.Transition.ID
			}
			index := transitionIndex(result.Timeline.Transitions, id)
			if index < 0 {
				err = fmt.Errorf("transition %q not found", id)
				break
			}
			tr := *op.Transition
			tr.ID = id
			result.Timeline.Transitions[index] = tr
		case "remove_transition":
			index := transitionIndex(result.Timeline.Transitions, op.ID)
			if index < 0 {
				err = fmt.Errorf("transition %q not found", op.ID)
				break
			}
			result.Timeline.Transitions = append(result.Timeline.Transitions[:index], result.Timeline.Transitions[index+1:]...)
			result.RemovedIDs = append(result.RemovedIDs, op.ID)
		default:
			err = fmt.Errorf("unknown timeline operation %q", op.Type)
		}
		if err != nil {
			return OperationResult{}, err
		}
	}
	normalized, err := normalizeTimeline(result.Timeline)
	if err != nil {
		return OperationResult{}, err
	}
	result.Timeline = normalized
	sort.Strings(result.CreatedIDs)
	sort.Strings(result.RemovedIDs)
	return result, nil
}

func timelineClipIndex(clips []TimelineClip, id string) int {
	for i := range clips {
		if clips[i].ID == id {
			return i
		}
	}
	return -1
}

func transitionIndex(items []TimelineTransition, id string) int {
	for i := range items {
		if items[i].ID == id {
			return i
		}
	}
	return -1
}

func newTimelineID(prefix string) string {
	var value [8]byte
	_, _ = rand.Read(value[:])
	return prefix + "-" + hex.EncodeToString(value[:])
}

type TimelineTransaction struct {
	mu           sync.Mutex
	store        *Store
	projectID    string
	baseRevision int
	doc          Timeline
	meta         CommitMeta
	dirty        bool
	mediaDirty   bool
	closed       bool
	baseMedia    map[string]string
	headTarget   *int
	checkpoints  []string
}

func (s *Store) BeginTimelineTransaction(projectID string, meta CommitMeta) (*TimelineTransaction, error) {
	doc, err := s.GetTimeline(projectID)
	if err != nil {
		return nil, err
	}
	history, err := s.History(projectID)
	if err != nil {
		return nil, err
	}
	var baseMedia map[string]string
	for _, rev := range history.Revisions {
		if rev.ID == history.Head {
			baseMedia = rev.Media
			break
		}
	}
	return &TimelineTransaction{store: s, projectID: projectID, baseRevision: doc.Revision, doc: doc, meta: normalizeMeta(meta), baseMedia: baseMedia}, nil
}

func (tx *TimelineTransaction) Get() Timeline {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	return tx.doc
}

// Focus moves the playhead and selection so the preview shows a just-placed clip.
func (tx *TimelineTransaction) Focus(id string, frame int) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.closed {
		return
	}
	if frame >= 0 {
		tx.doc.PlayheadFrame = frame
	}
	if strings.TrimSpace(id) != "" {
		tx.doc.SelectedID = id
	}
	tx.dirty = true
}

func (tx *TimelineTransaction) Apply(operations []TimelineOperation) (OperationResult, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.closed {
		return OperationResult{}, errors.New("timeline transaction is closed")
	}
	result, err := ApplyTimelineOperations(tx.doc, operations)
	if err != nil {
		return OperationResult{}, err
	}
	tx.doc = result.Timeline
	tx.dirty = true
	return result, nil
}

func (tx *TimelineTransaction) StageRestore(target int) (Timeline, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.closed {
		return Timeline{}, errors.New("timeline transaction is closed")
	}
	if tx.dirty || tx.mediaDirty {
		return Timeline{}, errors.New("restore must be the first mutation in a Director request")
	}
	p, err := tx.store.Get(tx.projectID)
	if err != nil {
		return Timeline{}, err
	}
	tx.store.mu.RLock()
	rev, err := readRevision(p, target)
	tx.store.mu.RUnlock()
	if err != nil {
		return Timeline{}, err
	}
	tx.doc = rev.Timeline
	tx.doc.Revision = target
	tx.headTarget = &target
	return tx.doc, nil
}

func (tx *TimelineTransaction) StageUndo() (Timeline, error) {
	history, err := tx.store.History(tx.projectID)
	if err != nil {
		return Timeline{}, err
	}
	if history.Head != tx.baseRevision {
		return Timeline{}, ErrRevisionConflict
	}
	for _, rev := range history.Revisions {
		if rev.ID == history.Head {
			if rev.ParentID == nil {
				return Timeline{}, ErrNoUndo
			}
			return tx.StageRestore(*rev.ParentID)
		}
	}
	return Timeline{}, ErrNoUndo
}

func (tx *TimelineTransaction) StageRedo(target int) (Timeline, error) {
	history, err := tx.store.History(tx.projectID)
	if err != nil {
		return Timeline{}, err
	}
	if history.Head != tx.baseRevision {
		return Timeline{}, ErrRevisionConflict
	}
	if len(history.RedoCandidates) == 0 {
		return Timeline{}, ErrNoRedo
	}
	if target < 0 {
		target = history.RedoCandidates[len(history.RedoCandidates)-1]
	}
	valid := false
	for _, candidate := range history.RedoCandidates {
		if candidate == target {
			valid = true
		}
	}
	if !valid {
		return Timeline{}, errors.New("revision is not a redo candidate")
	}
	return tx.StageRestore(target)
}

func (tx *TimelineTransaction) StageCheckpoint(name string) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 {
		return errors.New("checkpoint name must be 1-80 characters")
	}
	tx.checkpoints = append(tx.checkpoints, name)
	return nil
}

func (tx *TimelineTransaction) Commit() (Timeline, bool, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.closed {
		return Timeline{}, false, errors.New("timeline transaction is closed")
	}
	tx.closed = true
	if tx.headTarget != nil && !tx.dirty && !tx.mediaDirty {
		doc, err := tx.store.RestoreRevision(tx.projectID, *tx.headTarget, tx.baseRevision)
		if err == nil {
			for _, name := range tx.checkpoints {
				_ = tx.store.CreateCheckpoint(tx.projectID, name, doc.Revision)
			}
		}
		return doc, err == nil, err
	}
	if !tx.dirty && !tx.mediaDirty {
		for _, name := range tx.checkpoints {
			_ = tx.store.CreateCheckpoint(tx.projectID, name, tx.baseRevision)
		}
		return tx.doc, false, nil
	}
	doc, err := tx.store.saveTimelineCommit(tx.projectID, tx.doc, tx.baseRevision, tx.meta, tx.mediaDirty)
	if err != nil && tx.mediaDirty {
		if p, getErr := tx.store.Get(tx.projectID); getErr == nil {
			current, _ := snapshotMedia(p)
			_ = restoreMedia(p, tx.baseMedia, current)
		}
	}
	if err == nil {
		for _, name := range tx.checkpoints {
			_ = tx.store.CreateCheckpoint(tx.projectID, name, doc.Revision)
		}
	}
	return doc, err == nil, err
}

func (tx *TimelineTransaction) MarkMediaMutation() {
	tx.mu.Lock()
	if !tx.closed {
		tx.mediaDirty = true
	}
	tx.mu.Unlock()
}

func (tx *TimelineTransaction) SetChatID(chatID string) {
	tx.mu.Lock()
	if !tx.closed {
		tx.meta.ChatID = strings.TrimSpace(chatID)
	}
	tx.mu.Unlock()
}

func (tx *TimelineTransaction) Rollback() {
	tx.mu.Lock()
	if tx.mediaDirty {
		if p, err := tx.store.Get(tx.projectID); err == nil {
			current, _ := snapshotMedia(p)
			_ = restoreMedia(p, tx.baseMedia, current)
		}
	}
	tx.closed = true
	tx.mu.Unlock()
}
