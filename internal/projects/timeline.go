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
	"unicode/utf8"
)

const (
	timelineSchema     = 1
	timelineDefaultFPS = 24
	timelineMaxClips   = 2000
	timelineMaxName    = 200
	timelineMaxID      = 80
)

var (
	ErrInvalidTimeline = errors.New("invalid timeline")

	allowedTracks = map[string]string{
		"V1": "video",
		"V2": "title",
		"A1": "audio",
		"A2": "audio",
	}

	allowedMediaTypes = map[string]bool{
		"":      true,
		"video": true,
		"audio": true,
		"image": true,
	}
)

// Timeline is the on-disk sequence document. Times are integer frames at FPS
// so a save/load round-trip cannot accumulate float error.
type Timeline struct {
	Schema        int            `json:"schema"`
	FPS           int            `json:"fps"`
	Revision      int            `json:"revision"`
	PlayheadFrame int            `json:"playhead_frame"`
	SelectedID    string         `json:"selected_id,omitempty"`
	PxPerSecond   float64        `json:"px_per_second,omitempty"`
	UpdatedAt     time.Time      `json:"updated_at,omitempty"`
	Clips         []TimelineClip `json:"clips"`
}

// TimelineClip is one record-side item. SourceInFrame is the media in-point.
type TimelineClip struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	Track                string `json:"track"`
	Kind                 string `json:"kind"`
	StartFrame           int    `json:"start_frame"`
	DurationFrames       int    `json:"duration_frames"`
	SourceInFrame        int    `json:"source_in_frame"`
	SourceDurationFrames int    `json:"source_duration_frames,omitempty"`
	MediaPath            string `json:"media_path,omitempty"`
	MediaType            string `json:"media_type,omitempty"`
	Color                string `json:"color,omitempty"`
	WaveSeed             int    `json:"wave_seed,omitempty"`
}

func emptyTimeline() Timeline {
	return Timeline{
		Schema: timelineSchema,
		FPS:    timelineDefaultFPS,
		Clips:  []TimelineClip{},
	}
}

func (s *Store) GetTimeline(projectID string) (Timeline, error) {
	p, err := s.Get(projectID)
	if err != nil {
		return Timeline{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readTimeline(p)
}

func (s *Store) SaveTimeline(projectID string, doc Timeline) (Timeline, error) {
	p, err := s.Get(projectID)
	if err != nil {
		return Timeline{}, err
	}
	normalized, err := normalizeTimeline(doc)
	if err != nil {
		return Timeline{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := readTimeline(p)
	if err != nil {
		return Timeline{}, err
	}
	normalized.Revision = current.Revision + 1
	normalized.UpdatedAt = time.Now().UTC()
	if err := writeTimeline(p, normalized); err != nil {
		return Timeline{}, err
	}
	return normalized, nil
}

func timelinePath(p Project) string {
	return filepath.Join(p.Dir, ".parallax", "timeline.json")
}

func readTimeline(p Project) (Timeline, error) {
	b, err := os.ReadFile(timelinePath(p))
	if err != nil {
		if os.IsNotExist(err) {
			return emptyTimeline(), nil
		}
		return Timeline{}, err
	}
	var doc Timeline
	if err := json.Unmarshal(b, &doc); err != nil {
		return Timeline{}, fmt.Errorf("%w: %v", ErrInvalidTimeline, err)
	}
	if doc.Clips == nil {
		doc.Clips = []TimelineClip{}
	}
	if doc.Schema == 0 {
		doc.Schema = timelineSchema
	}
	if doc.FPS == 0 {
		doc.FPS = timelineDefaultFPS
	}
	return doc, nil
}

func writeTimeline(p Project, doc Timeline) error {
	dir := filepath.Join(p.Dir, ".parallax")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if doc.Clips == nil {
		doc.Clips = []TimelineClip{}
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := timelinePath(p) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, timelinePath(p))
}

func normalizeTimeline(doc Timeline) (Timeline, error) {
	if doc.Schema == 0 {
		doc.Schema = timelineSchema
	}
	if doc.Schema != timelineSchema {
		return Timeline{}, fmt.Errorf("%w: unsupported schema %d", ErrInvalidTimeline, doc.Schema)
	}
	if doc.FPS == 0 {
		doc.FPS = timelineDefaultFPS
	}
	if doc.FPS < 1 || doc.FPS > 240 {
		return Timeline{}, fmt.Errorf("%w: fps must be between 1 and 240", ErrInvalidTimeline)
	}
	if doc.PlayheadFrame < 0 {
		return Timeline{}, fmt.Errorf("%w: playhead cannot be negative", ErrInvalidTimeline)
	}
	if doc.PxPerSecond < 0 || doc.PxPerSecond > 240 {
		return Timeline{}, fmt.Errorf("%w: invalid zoom", ErrInvalidTimeline)
	}
	if doc.SelectedID != "" {
		if err := validateClipID(doc.SelectedID); err != nil {
			return Timeline{}, err
		}
	}
	if len(doc.Clips) > timelineMaxClips {
		return Timeline{}, fmt.Errorf("%w: too many clips", ErrInvalidTimeline)
	}

	seen := make(map[string]struct{}, len(doc.Clips))
	out := make([]TimelineClip, 0, len(doc.Clips))
	for _, clip := range doc.Clips {
		normalized, err := normalizeClip(clip)
		if err != nil {
			return Timeline{}, err
		}
		if _, ok := seen[normalized.ID]; ok {
			return Timeline{}, fmt.Errorf("%w: duplicate clip id %q", ErrInvalidTimeline, normalized.ID)
		}
		seen[normalized.ID] = struct{}{}
		out = append(out, normalized)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StartFrame != out[j].StartFrame {
			return out[i].StartFrame < out[j].StartFrame
		}
		return out[i].ID < out[j].ID
	})
	if doc.SelectedID != "" {
		if _, ok := seen[doc.SelectedID]; !ok {
			doc.SelectedID = ""
		}
	}
	doc.Clips = out
	return doc, nil
}

func normalizeClip(clip TimelineClip) (TimelineClip, error) {
	if err := validateClipID(clip.ID); err != nil {
		return TimelineClip{}, err
	}
	clip.Name = strings.TrimSpace(clip.Name)
	if clip.Name == "" {
		clip.Name = "Clip"
	}
	if utf8.RuneCountInString(clip.Name) > timelineMaxName {
		return TimelineClip{}, fmt.Errorf("%w: clip %q name is too long", ErrInvalidTimeline, clip.ID)
	}
	wantKind, ok := allowedTracks[clip.Track]
	if !ok {
		return TimelineClip{}, fmt.Errorf("%w: clip %q has unknown track %q", ErrInvalidTimeline, clip.ID, clip.Track)
	}
	if clip.Kind == "" {
		clip.Kind = wantKind
	}
	if clip.Kind != wantKind {
		return TimelineClip{}, fmt.Errorf("%w: clip %q kind %q does not match track %s", ErrInvalidTimeline, clip.ID, clip.Kind, clip.Track)
	}
	if clip.StartFrame < 0 || clip.SourceInFrame < 0 || clip.SourceDurationFrames < 0 {
		return TimelineClip{}, fmt.Errorf("%w: clip %q has a negative time", ErrInvalidTimeline, clip.ID)
	}
	if clip.DurationFrames < 1 {
		return TimelineClip{}, fmt.Errorf("%w: clip %q duration must be at least 1 frame", ErrInvalidTimeline, clip.ID)
	}
	if clip.SourceDurationFrames > 0 && clip.SourceInFrame >= clip.SourceDurationFrames {
		return TimelineClip{}, fmt.Errorf("%w: clip %q in-point is past the source", ErrInvalidTimeline, clip.ID)
	}
	if clip.SourceDurationFrames > 0 && clip.SourceInFrame+clip.DurationFrames > clip.SourceDurationFrames {
		clip.DurationFrames = clip.SourceDurationFrames - clip.SourceInFrame
		if clip.DurationFrames < 1 {
			return TimelineClip{}, fmt.Errorf("%w: clip %q has no source remaining", ErrInvalidTimeline, clip.ID)
		}
	}
	path, err := sanitizeMediaPath(clip.MediaPath)
	if err != nil {
		return TimelineClip{}, fmt.Errorf("%w: clip %q %v", ErrInvalidTimeline, clip.ID, err)
	}
	clip.MediaPath = path
	if !allowedMediaTypes[clip.MediaType] {
		return TimelineClip{}, fmt.Errorf("%w: clip %q has invalid media type", ErrInvalidTimeline, clip.ID)
	}
	if clip.Color != "" && !validColor(clip.Color) {
		return TimelineClip{}, fmt.Errorf("%w: clip %q has invalid color", ErrInvalidTimeline, clip.ID)
	}
	if clip.WaveSeed < 0 {
		clip.WaveSeed = 0
	}
	return clip, nil
}

func validateClipID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > timelineMaxID || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("%w: invalid clip id", ErrInvalidTimeline)
	}
	for _, r := range id {
		if r < 33 || r > 126 {
			return fmt.Errorf("%w: invalid clip id", ErrInvalidTimeline)
		}
	}
	return nil
}

func sanitizeMediaPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	path = filepath.ToSlash(path)
	if strings.HasPrefix(path, "/") || strings.Contains(path, "://") {
		return "", errors.New("media path must be project-relative")
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("media path escapes the project")
	}
	return clean, nil
}

func validColor(s string) bool {
	if len(s) != 4 && len(s) != 7 {
		return false
	}
	if s[0] != '#' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
