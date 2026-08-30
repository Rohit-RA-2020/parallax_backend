package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	media, err := snapshotMedia(p, nil)
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
	if s.pg != nil {
		return s.pgHistory(context.Background(), projectID)
	}
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

func (s *Store) pgHistory(ctx context.Context, projectID string) (History, error) {
	if _, err := s.Get(projectID); err != nil {
		return History{}, err
	}
	var head int
	if err := s.pg.QueryRow(ctx, `SELECT head_revision_no FROM projects WHERE id=$1 AND state='active'`, projectID).Scan(&head); err != nil {
		return History{}, err
	}
	rows, err := s.pg.Query(ctx, `SELECT revision_no,parent_revision_no,actor_type,summary,COALESCE(chat_id::text,''),created_at,timeline FROM project_revisions WHERE project_id=$1 ORDER BY revision_no`, projectID)
	if err != nil {
		return History{}, err
	}
	defer rows.Close()
	revs := []Revision{}
	byID := map[int]int{}
	for rows.Next() {
		var r Revision
		var parent *int
		var body []byte
		if err = rows.Scan(&r.ID, &parent, &r.Actor, &r.Summary, &r.ChatID, &r.CreatedAt, &body); err != nil {
			return History{}, err
		}
		r.ParentID = parent
		if err = json.Unmarshal(body, &r.Timeline); err != nil {
			return History{}, err
		}
		r.Media = map[string]string{}
		byID[r.ID] = len(revs)
		revs = append(revs, r)
	}
	if err = rows.Err(); err != nil {
		return History{}, err
	}
	for i := range revs {
		if revs[i].ParentID != nil {
			if p, ok := byID[*revs[i].ParentID]; ok {
				revs[p].Children = append(revs[p].Children, revs[i].ID)
			}
		}
	}
	cpRows, err := s.pg.Query(ctx, `SELECT revision_no,name::text FROM checkpoints WHERE project_id=$1 ORDER BY name`, projectID)
	if err != nil {
		return History{}, err
	}
	defer cpRows.Close()
	for cpRows.Next() {
		var id int
		var name string
		if cpRows.Scan(&id, &name) == nil {
			if i, ok := byID[id]; ok {
				revs[i].Checkpoints = append(revs[i].Checkpoints, name)
			}
		}
	}
	h := History{Head: head, Revisions: revs}
	if i, ok := byID[head]; ok {
		h.CanUndo = revs[i].ParentID != nil
		h.RedoCandidates = append([]int(nil), revs[i].Children...)
	}
	return h, nil
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
	if s.pg != nil {
		return s.restorePGRevision(context.Background(), projectID, target, expected)
	}
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

func (s *Store) restorePGRevision(ctx context.Context, projectID string, target, expected int) (Timeline, error) {
	if _, err := s.Get(projectID); err != nil {
		return Timeline{}, err
	}
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return Timeline{}, err
	}
	defer tx.Rollback(ctx)
	var head int
	query := `SELECT head_revision_no FROM projects WHERE id=$1 AND state='active'`
	args := []any{projectID}
	if s.ownerID != uuid.Nil {
		query += ` AND owner_id=$2`
		args = append(args, s.ownerID)
	}
	query += ` FOR UPDATE`
	if err = tx.QueryRow(ctx, query, args...).Scan(&head); errors.Is(err, pgx.ErrNoRows) {
		return Timeline{}, ErrNotFound
	} else if err != nil {
		return Timeline{}, err
	}
	if expected >= 0 && expected != head {
		return Timeline{}, ErrRevisionConflict
	}
	var body []byte
	if err = tx.QueryRow(ctx, `SELECT timeline FROM project_revisions WHERE project_id=$1 AND revision_no=$2`, projectID, target).Scan(&body); errors.Is(err, pgx.ErrNoRows) {
		return Timeline{}, ErrNotFound
	} else if err != nil {
		return Timeline{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE assets SET deleted_at=now(),updated_at=now() WHERE project_id=$1`, projectID); err != nil {
		return Timeline{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE assets a SET current_version_id=ra.asset_version_id,logical_path=ra.logical_path,display_name=ra.display_name,deleted_at=CASE WHEN ra.present THEN NULL ELSE now() END,updated_at=now() FROM revision_assets ra WHERE ra.project_id=$1 AND ra.revision_no=$2 AND a.id=ra.asset_id`, projectID, target); err != nil {
		return Timeline{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE projects SET head_revision_no=$2,updated_at=now() WHERE id=$1`, projectID, target); err != nil {
		return Timeline{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Timeline{}, err
	}
	var doc Timeline
	if err = json.Unmarshal(body, &doc); err != nil {
		return Timeline{}, err
	}
	doc.Revision = target
	return doc, nil
}

func (s *Store) Undo(projectID string, expected int) (Timeline, error) {
	if s.pg != nil {
		h, err := s.History(projectID)
		if err != nil {
			return Timeline{}, err
		}
		if expected >= 0 && h.Head != expected {
			return Timeline{}, ErrRevisionConflict
		}
		for _, r := range h.Revisions {
			if r.ID == h.Head && r.ParentID != nil {
				return s.RestoreRevision(projectID, *r.ParentID, h.Head)
			}
		}
		return Timeline{}, ErrNoUndo
	}
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
	if s.pg != nil {
		h, err := s.History(projectID)
		if err != nil {
			return Timeline{}, err
		}
		if expected >= 0 && h.Head != expected {
			return Timeline{}, ErrRevisionConflict
		}
		candidates := h.RedoCandidates
		if target < 0 {
			if len(candidates) != 1 {
				return Timeline{}, ErrNoRedo
			}
			target = candidates[0]
		}
		for _, id := range candidates {
			if id == target {
				return s.RestoreRevision(projectID, target, h.Head)
			}
		}
		return Timeline{}, ErrNoRedo
	}
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
	if s.pg != nil {
		if _, err := s.Get(projectID); err != nil {
			return err
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return errors.New("checkpoint name is required")
		}
		if revision < 0 {
			h, err := s.History(projectID)
			if err != nil {
				return err
			}
			revision = h.Head
		}
		_, err := s.pg.Exec(context.Background(), `INSERT INTO checkpoints(id,project_id,revision_no,name) VALUES($1,$2,$3,$4)`, uuid.New(), projectID, revision, name)
		return err
	}
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
	if s.pg != nil {
		if _, err := s.Get(projectID); err != nil {
			return err
		}
		result, err := s.pg.Exec(context.Background(), `UPDATE checkpoints SET name=$3,updated_at=now() WHERE project_id=$1 AND name=$2`, projectID, oldName, newName)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	}
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
	if s.pg != nil {
		if _, err := s.Get(projectID); err != nil {
			return err
		}
		result, err := s.pg.Exec(context.Background(), `DELETE FROM checkpoints WHERE project_id=$1 AND name=$2`, projectID, name)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	}
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
