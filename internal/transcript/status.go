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
	StateDescribing   = "describing"
	StateIndexing     = "indexing"
	StateReady        = "ready"
	StateIndexFailed  = "index_failed"
	StateFailed       = "failed"
	StateSkipped      = "skipped"
)

const (
	TimingUpload     = "upload"
	TimingQueue      = "queue"
	TimingExtract    = "extract"
	TimingTranscribe = "transcribe"
	TimingTranslate  = "translate"
	TimingDescribe   = "describe"
	TimingIndex      = "index"
)

// JobTimings is a wall-clock breakdown of one ingest/index run, in milliseconds.
type JobTimings struct {
	UploadMs     int64  `json:"upload_ms,omitempty"`
	QueueMs      int64  `json:"queue_ms,omitempty"`
	ExtractMs    int64  `json:"extract_ms,omitempty"`
	TranscribeMs int64  `json:"transcribe_ms,omitempty"`
	TranslateMs  int64  `json:"translate_ms,omitempty"`
	DescribeMs   int64  `json:"describe_ms,omitempty"`
	IndexMs      int64  `json:"index_ms,omitempty"`
	TotalMs      int64  `json:"total_ms,omitempty"`
	Cached       bool   `json:"cached,omitempty"`
	Model        string `json:"model,omitempty"`
	Device       string `json:"device,omitempty"`
}

func (t JobTimings) sum() int64 {
	return t.UploadMs + t.QueueMs + t.ExtractMs + t.TranscribeMs + t.TranslateMs + t.DescribeMs + t.IndexMs
}

func finalizeTotal(t *JobTimings, started time.Time) {
	if t == nil {
		return
	}
	if sum := t.sum(); sum > t.TotalMs {
		t.TotalMs = sum
	}
	if t.TotalMs > 0 {
		return
	}
	if ms := sinceMs(started); ms > 0 {
		t.TotalMs = ms
	}
}

func isTerminalState(state string) bool {
	switch state {
	case StateReady, StateIndexFailed, StateFailed, StateSkipped:
		return true
	default:
		return false
	}
}

// JobStatus is the public transcript/index state for one media file.
type JobStatus struct {
	Path           string     `json:"path"`
	State          string     `json:"state"`
	Hash           string     `json:"hash,omitempty"`
	Error          string     `json:"error,omitempty"`
	Progress       string     `json:"progress,omitempty"`
	At             float64    `json:"at,omitempty"`
	Duration       float64    `json:"duration,omitempty"`
	Timings        JobTimings `json:"timings,omitempty"`
	CanDescribe    bool       `json:"can_describe,omitempty"`
	StartedAt      time.Time  `json:"started_at,omitempty"`
	StageStartedAt time.Time  `json:"stage_started_at,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at"`
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
	state = strings.TrimSpace(state)
	if state == "" {
		return
	}
	x.patchStatus(projectID, rel, true, func(st *JobStatus) {
		st.State = state
		st.Error = strings.TrimSpace(errMsg)
		if state != StateTranscribing && state != StateDescribing {
			st.Progress = ""
		}
		if isTerminalState(state) {
			finalizeTotal(&st.Timings, st.StartedAt)
		}
	})
}

// NoteUpload records how long it took to receive and write one uploaded file.
func (x *Indexer) NoteUpload(projectID, rel string, uploadMs int64) {
	if uploadMs < 1 {
		uploadMs = 1
	}
	x.patchStatus(projectID, rel, true, func(st *JobStatus) {
		st.Timings = JobTimings{UploadMs: uploadMs}
		backdate := time.Duration(uploadMs) * time.Millisecond
		if st.StartedAt.IsZero() {
			st.StartedAt = time.Now().UTC().Add(-backdate)
		} else {
			st.StartedAt = st.StartedAt.Add(-backdate)
		}
		if st.State == "" {
			st.State = StateQueued
		}
	})
}

// AddTiming records or accumulates one stage duration in milliseconds.
func (x *Indexer) AddTiming(projectID, rel, kind string, ms int64) {
	if ms < 1 {
		ms = 1
	}
	x.patchStatus(projectID, rel, true, func(st *JobStatus) {
		switch kind {
		case TimingUpload:
			st.Timings.UploadMs = ms
		case TimingQueue:
			st.Timings.QueueMs = ms
		case TimingExtract:
			st.Timings.ExtractMs = ms
			st.StageStartedAt = time.Now().UTC()
		case TimingTranscribe:
			st.Timings.TranscribeMs = ms
		case TimingTranslate:
			st.Timings.TranslateMs = ms
		case TimingDescribe:
			st.Timings.DescribeMs = ms
		case TimingIndex:
			st.Timings.IndexMs += ms
		}
	})
}

// SetTimingMeta stamps the ASR model/device used for this run.
func (x *Indexer) SetTimingMeta(projectID, rel, model, device string) {
	model = strings.TrimSpace(model)
	device = strings.TrimSpace(device)
	if model == "" && device == "" {
		return
	}
	x.patchStatus(projectID, rel, true, func(st *JobStatus) {
		if model != "" {
			st.Timings.Model = model
		}
		if device != "" {
			st.Timings.Device = device
		}
	})
}

// NoteCached marks that speech was reused instead of running Whisper.
func (x *Indexer) NoteCached(projectID, rel string) {
	x.patchStatus(projectID, rel, true, func(st *JobStatus) {
		st.Timings.Cached = true
	})
}

// stampQueue records how long the file waited after Enqueue.
func (x *Indexer) stampQueue(projectID, rel string) {
	if x == nil {
		return
	}
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	st := x.lookup(projectID, rel)
	if st.State != StateQueued {
		return
	}
	start := st.StageStartedAt
	if start.IsZero() {
		start = st.UpdatedAt
	}
	if ms := sinceMs(start); ms > 0 {
		x.AddTiming(projectID, rel, TimingQueue, ms)
	}
}

// MarkCaptionProgress records how many stills or scenes have been described.
func (x *Indexer) MarkCaptionProgress(projectID, rel string, done, total int) {
	if done < 0 {
		done = 0
	}
	progress := ""
	if total > 0 {
		progress = fmt.Sprintf("%d / %d", done, total)
	}
	x.patchStatus(projectID, rel, true, func(st *JobStatus) {
		st.State = StateDescribing
		st.Progress = progress
	})
}

// MarkProgress updates live transcribe position without rewriting disk.
func (x *Indexer) MarkProgress(projectID, rel string, at, duration float64) {
	x.patchStatus(projectID, rel, false, func(st *JobStatus) {
		st.State = StateTranscribing
		st.Progress = formatClock(at) + " / " + formatClock(duration)
		st.At = at
		if duration > 0 {
			st.Duration = duration
		}
	})
}

func (x *Indexer) patchStatus(projectID, rel string, persist bool, fn func(*JobStatus)) {
	if x == nil || fn == nil {
		return
	}
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if rel == "" {
		return
	}
	x.mu.Lock()
	x.ensureLive()
	key := statusKey(projectID, rel)
	_, liveOK := x.live[key]
	x.mu.Unlock()

	var disk JobStatus
	var haveDisk bool
	var projectDir string
	if x.Projects != nil && (persist || !liveOK) {
		if project, err := x.Projects.Get(projectID); err == nil {
			projectDir = project.Dir
			if !liveOK {
				if st, ok := readStatusFile(project.Dir)[rel]; ok {
					disk = st
					haveDisk = true
				}
			}
		}
	}
	now := time.Now().UTC()
	x.mu.Lock()
	x.ensureLive()
	st, ok := x.live[key]
	if !ok && haveDisk {
		st = disk
	}
	if st.Path == "" {
		st.Path = rel
	}
	prevState := st.State
	started := st.StartedAt
	st.UpdatedAt = now
	fn(&st)
	if st.StartedAt.IsZero() {
		if !started.IsZero() {
			st.StartedAt = started
		} else {
			st.StartedAt = now
		}
	}
	if st.State != prevState || st.StageStartedAt.IsZero() {
		st.StageStartedAt = now
	}
	x.live[key] = st
	x.mu.Unlock()
	if persist && projectDir != "" {
		x.persistStatus(projectDir, st)
	}
}

func (x *Indexer) lookup(projectID, rel string) JobStatus {
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if x == nil || rel == "" {
		return JobStatus{Path: rel}
	}
	x.mu.Lock()
	if x.live != nil {
		if st, ok := x.live[statusKey(projectID, rel)]; ok {
			x.mu.Unlock()
			return st
		}
	}
	x.mu.Unlock()
	if x.Projects != nil {
		if project, err := x.Projects.Get(projectID); err == nil {
			if st, ok := readStatusFile(project.Dir)[rel]; ok {
				return st
			}
		}
	}
	return JobStatus{Path: rel}
}

func (x *Indexer) setLive(projectID, rel string, st JobStatus) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.ensureLive()
	x.live[statusKey(projectID, rel)] = st
}

func sinceMs(start time.Time) int64 {
	if start.IsZero() {
		return 0
	}
	ms := time.Since(start).Milliseconds()
	if ms < 1 {
		return 1
	}
	return ms
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
	x.removeStatus(project.Dir, rel)
}

func (x *Indexer) persistStatus(projectDir string, st JobStatus) {
	if x == nil {
		_ = writeStatus(projectDir, st)
		return
	}
	x.diskMu.Lock()
	defer x.diskMu.Unlock()
	_ = writeStatus(projectDir, st)
}

func (x *Indexer) removeStatus(projectDir, rel string) {
	if x == nil {
		_ = deleteStatus(projectDir, rel)
		return
	}
	x.diskMu.Lock()
	defer x.diskMu.Unlock()
	_ = deleteStatus(projectDir, rel)
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
