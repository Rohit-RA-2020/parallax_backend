package preview

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	StateOriginal = "original"
	StateQueued   = "queued"
	StateBuilding = "building"
	StateReady    = "ready"
	StateFailed   = "failed"
)

// Status is the public preview-proxy state for one media file.
type Status struct {
	Path       string    `json:"path"`
	State      string    `json:"state"`
	URLPath    string    `json:"url_path,omitempty"`
	PosterPath string    `json:"poster_path,omitempty"`
	Progress   string    `json:"progress,omitempty"`
	Error      string    `json:"error,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Codec      string    `json:"codec,omitempty"`
	Encoder    string    `json:"encoder,omitempty"`
	Device     string    `json:"device,omitempty"`
	Hardware   bool      `json:"hardware"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func statusFile(projectDir string) string {
	return filepath.Join(projectDir, ".parallax", "preview-status.json")
}

func statusKey(projectID, rel string) string {
	return projectID + "\n" + filepath.ToSlash(strings.TrimSpace(rel))
}

func (b *Builder) Mark(projectID, rel string, st Status) {
	if b == nil {
		return
	}
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if rel == "" {
		return
	}
	st.Path = rel
	st.UpdatedAt = time.Now().UTC()
	b.mu.Lock()
	if b.live == nil {
		b.live = map[string]Status{}
	}
	b.live[statusKey(projectID, rel)] = st
	b.mu.Unlock()
	if b.Projects == nil {
		return
	}
	if project, err := b.Projects.Get(projectID); err == nil {
		b.persist(project.Dir, st)
	}
}

func (b *Builder) Clear(projectID, rel string) {
	if b == nil {
		return
	}
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	b.mu.Lock()
	if b.live != nil {
		delete(b.live, statusKey(projectID, rel))
	}
	b.mu.Unlock()
	if b.Projects == nil {
		return
	}
	project, err := b.Projects.Get(projectID)
	if err != nil {
		return
	}
	b.diskMu.Lock()
	defer b.diskMu.Unlock()
	all := readStatusFile(project.Dir)
	delete(all, rel)
	_ = saveStatusFile(project.Dir, all)
}

func (b *Builder) Statuses(projectID string) map[string]Status {
	out := map[string]Status{}
	if b == nil {
		return out
	}
	if b.Projects != nil {
		if project, err := b.Projects.Get(projectID); err == nil {
			for path, st := range readStatusFile(project.Dir) {
				out[path] = st
			}
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	prefix := projectID + "\n"
	for key, st := range b.live {
		if strings.HasPrefix(key, prefix) {
			out[strings.TrimPrefix(key, prefix)] = st
		}
	}
	return out
}

func (b *Builder) persist(projectDir string, st Status) {
	b.diskMu.Lock()
	defer b.diskMu.Unlock()
	all := readStatusFile(projectDir)
	all[st.Path] = st
	_ = saveStatusFile(projectDir, all)
}

func readStatusFile(projectDir string) map[string]Status {
	out := map[string]Status{}
	body, err := os.ReadFile(statusFile(projectDir))
	if err != nil {
		return out
	}
	_ = json.Unmarshal(body, &out)
	if out == nil {
		return map[string]Status{}
	}
	return out
}

func saveStatusFile(projectDir string, all map[string]Status) error {
	if err := os.MkdirAll(filepath.Join(projectDir, ".parallax"), 0o700); err != nil {
		return err
	}
	if all == nil {
		all = map[string]Status{}
	}
	body, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Join(projectDir, ".parallax"), ".preview-status-*")
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
	if _, err := tmp.Write(body); err != nil {
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

func formatClock(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	total := int(sec + 0.5)
	return fmt.Sprintf("%d:%02d", total/60, total%60)
}
