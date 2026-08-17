package transcript

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	StateQueued       = "queued"
	StateTranscribing = "transcribing"
	StateTranslating  = "translating"
	StateIndexing     = "indexing"
	StateReady        = "ready"
	StateIndexFailed  = "index_failed"
	StateFailed       = "failed"
	StateSkipped      = "skipped"
)

// JobStatus is the public transcript/index state for one media file.
type JobStatus struct {
	Path      string    `json:"path"`
	State     string    `json:"state"`
	Hash      string    `json:"hash,omitempty"`
	Error     string    `json:"error,omitempty"`
	Progress  string    `json:"progress,omitempty"`
	At        float64   `json:"at,omitempty"`
	Duration  float64   `json:"duration,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (x *Indexer) ensureLive() {
	if x.live == nil {
		x.live = map[string]JobStatus{}
	}
}

func statusKey(projectID, rel string) string {
	return projectID + "\n" + filepath.ToSlash(strings.TrimSpace(rel))
}

func statusFile(projectDir string) string {
	return filepath.Join(projectDir, ".parallax", "index-status.json")
}

// Mark records a file's index state in memory and on disk.
func (x *Indexer) Mark(projectID, rel, state, errMsg string) {
	if x == nil || x.Projects == nil {
		return
	}
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if rel == "" || strings.TrimSpace(state) == "" {
		return
	}
	project, err := x.Projects.Get(projectID)
	if err != nil {
		return
	}
	st := JobStatus{
		Path:      rel,
		State:     state,
		Error:     strings.TrimSpace(errMsg),
		UpdatedAt: time.Now().UTC(),
	}
	x.setLive(projectID, rel, st)
	_ = writeStatus(project.Dir, st)
}

// MarkProgress updates live transcribe position without rewriting disk.
func (x *Indexer) MarkProgress(projectID, rel string, at, duration float64) {
	if x == nil {
		return
	}
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if rel == "" {
		return
	}
	st := JobStatus{
		Path:      rel,
		State:     StateTranscribing,
		Progress:  formatClock(at) + " / " + formatClock(duration),
		At:        at,
		Duration:  duration,
		UpdatedAt: time.Now().UTC(),
	}
	x.setLive(projectID, rel, st)
}

func (x *Indexer) setLive(projectID, rel string, st JobStatus) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.ensureLive()
	x.live[statusKey(projectID, rel)] = st
}

func formatClock(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	total := int(sec + 0.5)
	return fmt.Sprintf("%d:%02d", total/60, total%60)
}

// Clear removes a file's stored index status.
func (x *Indexer) Clear(projectID, rel string) {
	if x == nil || x.Projects == nil {
		return
	}
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	x.mu.Lock()
	if x.live != nil {
		delete(x.live, statusKey(projectID, rel))
	}
	x.mu.Unlock()
	project, err := x.Projects.Get(projectID)
	if err != nil {
		return
	}
	_ = deleteStatus(project.Dir, rel)
}

func (x *Indexer) clearProject(projectID string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.live == nil {
		return
	}
	prefix := projectID + "\n"
	for key := range x.live {
		if strings.HasPrefix(key, prefix) {
			delete(x.live, key)
		}
	}
}

// Statuses returns the latest known state for every path in the project.
func (x *Indexer) Statuses(projectID string) map[string]JobStatus {
	out := map[string]JobStatus{}
	if x == nil || x.Projects == nil {
		return out
	}
	if project, err := x.Projects.Get(projectID); err == nil {
		for path, st := range readStatusFile(project.Dir) {
			out[path] = st
		}
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	prefix := projectID + "\n"
	for key, st := range x.live {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		out[strings.TrimPrefix(key, prefix)] = st
	}
	return out
}

func writeStatus(projectDir string, st JobStatus) error {
	all := readStatusFile(projectDir)
	all[st.Path] = st
	return saveStatusFile(projectDir, all)
}

func deleteStatus(projectDir, rel string) error {
	all := readStatusFile(projectDir)
	delete(all, rel)
	return saveStatusFile(projectDir, all)
}

func readStatusFile(projectDir string) map[string]JobStatus {
	out := map[string]JobStatus{}
	b, err := os.ReadFile(statusFile(projectDir))
	if err != nil {
		return out
	}
	_ = json.Unmarshal(b, &out)
	if out == nil {
		return map[string]JobStatus{}
	}
	return out
}

func saveStatusFile(projectDir string, all map[string]JobStatus) error {
	if err := os.MkdirAll(filepath.Join(projectDir, ".parallax"), 0o700); err != nil {
		return err
	}
	if all == nil {
		all = map[string]JobStatus{}
	}
	b, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Join(projectDir, ".parallax"), ".index-status-*")
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
	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, statusFile(projectDir)); err != nil {
		return err
	}
	ok = true
	return nil
}
