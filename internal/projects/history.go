package projects

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	ErrRevisionConflict = errors.New("timeline revision conflict")
	ErrNoUndo           = errors.New("nothing to undo")
	ErrNoRedo           = errors.New("nothing to redo")
)

type Revision struct {
	ID          int               `json:"id"`
	ParentID    *int              `json:"parent_id,omitempty"`
	Actor       string            `json:"actor"`
	Summary     string            `json:"summary"`
	ChatID      string            `json:"chat_id,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	Timeline    Timeline          `json:"timeline"`
	Media       map[string]string `json:"media,omitempty"`
	Children    []int             `json:"children,omitempty"`
	Checkpoints []string          `json:"checkpoints,omitempty"`
}

type History struct {
	Head           int        `json:"head"`
	CanUndo        bool       `json:"can_undo"`
	RedoCandidates []int      `json:"redo_candidates"`
	Revisions      []Revision `json:"revisions"`
}

type CommitMeta struct {
	Actor   string
	Summary string
	ChatID  string
}

func historyDir(p Project) string      { return filepath.Join(p.Dir, ".parallax", "history") }
func revisionsDir(p Project) string    { return filepath.Join(historyDir(p), "revisions") }
func headPath(p Project) string        { return filepath.Join(historyDir(p), "HEAD") }
func checkpointsPath(p Project) string { return filepath.Join(historyDir(p), "checkpoints.json") }

func normalizeMeta(meta CommitMeta) CommitMeta {
	meta.Actor = strings.TrimSpace(meta.Actor)
	if meta.Actor != "agent" && meta.Actor != "system" {
		meta.Actor = "human"
	}
	meta.Summary = strings.TrimSpace(meta.Summary)
	if meta.Summary == "" {
		meta.Summary = "Updated timeline"
	}
	if len(meta.Summary) > 240 {
		meta.Summary = meta.Summary[:240]
	}
	meta.ChatID = strings.TrimSpace(meta.ChatID)
	return meta
}

func ensureHistory(p Project, current Timeline) error {
	if _, err := os.Stat(headPath(p)); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(revisionsDir(p), 0o700); err != nil {
		return err
	}
	current.Revision = max(0, current.Revision)
	base := Revision{
		ID: current.Revision, Actor: "system", Summary: "Initial project state",
		CreatedAt: time.Now().UTC(), Timeline: current,
	}
	media, err := snapshotMedia(p)
	if err != nil {
		return err
	}
	base.Media = media
	if err := writeRevision(p, base); err != nil {
		return err
	}
	return writeIntAtomic(headPath(p), base.ID)
}

func writeRevision(p Project, rev Revision) error {
	b, err := json.MarshalIndent(rev, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(revisionsDir(p), fmt.Sprintf("%012d.json", rev.ID))
	return writeBytesAtomic(path, b, 0o600)
}

func readRevision(p Project, id int) (Revision, error) {
	b, err := os.ReadFile(filepath.Join(revisionsDir(p), fmt.Sprintf("%012d.json", id)))
	if err != nil {
		if os.IsNotExist(err) {
			return Revision{}, ErrNotFound
		}
		return Revision{}, err
	}
	var rev Revision
	if err := json.Unmarshal(b, &rev); err != nil {
		return Revision{}, err
	}
	if rev.Timeline.Clips == nil {
		rev.Timeline.Clips = []TimelineClip{}
	}
	return rev, nil
}

func readHead(p Project) (int, error) {
	b, err := os.ReadFile(headPath(p))
	if err != nil {
		return 0, err
	}
	var id int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &id); err != nil {
		return 0, err
	}
	return id, nil
}

func writeIntAtomic(path string, value int) error {
	return writeBytesAtomic(path, []byte(fmt.Sprintf("%d\n", value)), 0o600)
}

func writeBytesAtomic(path string, value []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(value); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func listRevisions(p Project) ([]Revision, error) {
	entries, err := os.ReadDir(revisionsDir(p))
	if err != nil {
		return nil, err
	}
	var out []Revision
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		var id int
		if _, err := fmt.Sscanf(strings.TrimSuffix(entry.Name(), ".json"), "%d", &id); err != nil {
			continue
		}
		rev, err := readRevision(p, id)
		if err != nil {
			return nil, err
		}
		out = append(out, rev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func readCheckpoints(p Project) (map[string]int, error) {
	b, err := os.ReadFile(checkpointsPath(p))
	if os.IsNotExist(err) {
		return map[string]int{}, nil
	}
	if err != nil {
		return nil, err
	}
	var out map[string]int
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]int{}
	}
	return out, nil
}

func (s *Store) History(projectID string) (History, error) {
	p, err := s.Get(projectID)
	if err != nil {
		return History{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := readTimeline(p)
	if err != nil {
		return History{}, err
	}
	if err := ensureHistory(p, current); err != nil {
		return History{}, err
	}
	return buildHistory(p)
}

func buildHistory(p Project) (History, error) {
	head, err := readHead(p)
	if err != nil {
		return History{}, err
	}
	revs, err := listRevisions(p)
	if err != nil {
		return History{}, err
	}
	checkpoints, err := readCheckpoints(p)
	if err != nil {
		return History{}, err
	}
	byID := make(map[int]*Revision, len(revs))
	for i := range revs {
		byID[revs[i].ID] = &revs[i]
	}
	for i := range revs {
		if revs[i].ParentID != nil {
			if parent := byID[*revs[i].ParentID]; parent != nil {
				parent.Children = append(parent.Children, revs[i].ID)
			}
		}
	}
	for name, id := range checkpoints {
		if rev := byID[id]; rev != nil {
			rev.Checkpoints = append(rev.Checkpoints, name)
		}
	}
	for i := range revs {
		sort.Ints(revs[i].Children)
		sort.Strings(revs[i].Checkpoints)
	}
	h := History{Head: head, Revisions: revs}
	if rev := byID[head]; rev != nil {
		h.CanUndo = rev.ParentID != nil
		h.RedoCandidates = append([]int(nil), rev.Children...)
	}
	return h, nil
}

func (s *Store) RestoreRevision(projectID string, target, expected int) (Timeline, error) {
	p, err := s.Get(projectID)
	if err != nil {
		return Timeline{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := readTimeline(p)
	if err != nil {
		return Timeline{}, err
	}
	if err := ensureHistory(p, current); err != nil {
		return Timeline{}, err
	}
	head, err := readHead(p)
	if err != nil {
		return Timeline{}, err
	}
	if expected >= 0 && expected != head {
		return Timeline{}, fmt.Errorf("%w: expected %d, current %d", ErrRevisionConflict, expected, head)
	}
	rev, err := readRevision(p, target)
	if err != nil {
		return Timeline{}, err
	}
	doc := rev.Timeline
	doc.Revision = rev.ID
	doc.UpdatedAt = time.Now().UTC()
	currentRev, _ := readRevision(p, head)
	if err := restoreMedia(p, rev.Media, currentRev.Media); err != nil {
		return Timeline{}, err
	}
	if err := writeTimeline(p, doc); err != nil {
		return Timeline{}, err
	}
	if err := writeIntAtomic(headPath(p), rev.ID); err != nil {
		return Timeline{}, err
	}
	return doc, nil
}

func (s *Store) Undo(projectID string, expected int) (Timeline, error) {
	h, err := s.History(projectID)
	if err != nil {
		return Timeline{}, err
	}
	if expected >= 0 && h.Head != expected {
		return Timeline{}, ErrRevisionConflict
	}
	var head Revision
	for _, rev := range h.Revisions {
		if rev.ID == h.Head {
			head = rev
			break
		}
	}
	if head.ParentID == nil {
		return Timeline{}, ErrNoUndo
	}
	return s.RestoreRevision(projectID, *head.ParentID, h.Head)
}

func (s *Store) Redo(projectID string, expected, target int) (Timeline, error) {
	h, err := s.History(projectID)
	if err != nil {
		return Timeline{}, err
	}
	if expected >= 0 && h.Head != expected {
		return Timeline{}, ErrRevisionConflict
	}
	candidates := h.RedoCandidates
	if len(candidates) == 0 {
		return Timeline{}, ErrNoRedo
	}
	if target < 0 {
		target = candidates[len(candidates)-1]
	}
	valid := false
	for _, id := range candidates {
		if id == target {
			valid = true
		}
	}
	if !valid {
		return Timeline{}, errors.New("revision is not a redo candidate")
	}
	return s.RestoreRevision(projectID, target, h.Head)
}

func (s *Store) CreateCheckpoint(projectID, name string, revision int) error {
	p, err := s.Get(projectID)
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 {
		return errors.New("checkpoint name must be 1-80 characters")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := readTimeline(p)
	if err != nil {
		return err
	}
	if err := ensureHistory(p, current); err != nil {
		return err
	}
	if revision < 0 {
		revision, err = readHead(p)
		if err != nil {
			return err
		}
	}
	if _, err := readRevision(p, revision); err != nil {
		return err
	}
	items, err := readCheckpoints(p)
	if err != nil {
		return err
	}
	items[name] = revision
	b, _ := json.MarshalIndent(items, "", "  ")
	return writeBytesAtomic(checkpointsPath(p), b, 0o600)
}

func (s *Store) RenameCheckpoint(projectID, oldName, newName string) error {
	p, err := s.Get(projectID)
	if err != nil {
		return err
	}
	oldName, newName = strings.TrimSpace(oldName), strings.TrimSpace(newName)
	if newName == "" || len(newName) > 80 {
		return errors.New("checkpoint name must be 1-80 characters")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := readCheckpoints(p)
	if err != nil {
		return err
	}
	revision, ok := items[oldName]
	if !ok {
		return ErrNotFound
	}
	delete(items, oldName)
	items[newName] = revision
	b, _ := json.MarshalIndent(items, "", "  ")
	return writeBytesAtomic(checkpointsPath(p), b, 0o600)
}

func (s *Store) DeleteCheckpoint(projectID, name string) error {
	p, err := s.Get(projectID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := readCheckpoints(p)
	if err != nil {
		return err
	}
	if _, ok := items[name]; !ok {
		return ErrNotFound
	}
	delete(items, name)
	b, _ := json.MarshalIndent(items, "", "  ")
	return writeBytesAtomic(checkpointsPath(p), b, 0o600)
}
