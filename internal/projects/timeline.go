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
	timelineSchema     = 2
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
		"C1": "caption",
		"A1": "audio",
		"A2": "audio",
	}

	allowedMediaTypes = map[string]bool{
		"":         true,
		"video":    true,
		"audio":    true,
		"image":    true,
		"subtitle": true,
	}
)

// Timeline is the on-disk sequence document. Times are integer frames at FPS
// so a save/load round-trip cannot accumulate float error.
type Timeline struct {
	Schema        int                  `json:"schema"`
	FPS           int                  `json:"fps"`
	Revision      int                  `json:"revision"`
	PlayheadFrame int                  `json:"playhead_frame"`
	SelectedID    string               `json:"selected_id,omitempty"`
	PxPerSecond   float64              `json:"px_per_second,omitempty"`
	UpdatedAt     time.Time            `json:"updated_at,omitempty"`
	Canvas        TimelineCanvas       `json:"canvas"`
	Clips         []TimelineClip       `json:"clips"`
	Transitions   []TimelineTransition `json:"transitions,omitempty"`
}

type TimelineCanvas struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type TimelineTransform struct {
	X          float64 `json:"x,omitempty"`
	Y          float64 `json:"y,omitempty"`
	AnchorX    float64 `json:"anchor_x,omitempty"`
	AnchorY    float64 `json:"anchor_y,omitempty"`
	ScaleX     float64 `json:"scale_x,omitempty"`
	ScaleY     float64 `json:"scale_y,omitempty"`
	Rotation   float64 `json:"rotation,omitempty"`
	Opacity    float64 `json:"opacity,omitempty"`
	CropTop    float64 `json:"crop_top,omitempty"`
	CropRight  float64 `json:"crop_right,omitempty"`
	CropBottom float64 `json:"crop_bottom,omitempty"`
	CropLeft   float64 `json:"crop_left,omitempty"`
}

type TimelinePlayback struct {
	Rate          float64 `json:"rate,omitempty"`
	PreservePitch bool    `json:"preserve_pitch,omitempty"`
}

type TimelineAudio struct {
	VolumeDB float64 `json:"volume_db,omitempty"`
	Muted    bool    `json:"muted,omitempty"`
	Pan      float64 `json:"pan,omitempty"`
}

type TimelineColor struct {
	Exposure    float64 `json:"exposure,omitempty"`
	Contrast    float64 `json:"contrast,omitempty"`
	Saturation  float64 `json:"saturation,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	Tint        float64 `json:"tint,omitempty"`
}

type TimelineTitle struct {
	Text        string  `json:"text"`
	FontFamily  string  `json:"font_family,omitempty"`
	FontSize    float64 `json:"font_size,omitempty"`
	FontWeight  int     `json:"font_weight,omitempty"`
	Align       string  `json:"align,omitempty"`
	Fill        string  `json:"fill,omitempty"`
	Stroke      string  `json:"stroke,omitempty"`
	StrokeWidth float64 `json:"stroke_width,omitempty"`
	Background  string  `json:"background,omitempty"`
}

// TimelineCaptions points a C1 clip at a timed subtitle file. Cue times are
// source-media seconds so they stay correct when the linked video is trimmed.
type TimelineCaptions struct {
	Language string `json:"language,omitempty"`
	Source   string `json:"source,omitempty"`
}

type TimelineKeyframe struct {
	Property string  `json:"property"`
	Frame    int     `json:"frame"`
	Value    float64 `json:"value"`
	Easing   string  `json:"easing,omitempty"`
}

type TimelineTransition struct {
	ID             string `json:"id"`
	Type           string `json:"type"`
	FromID         string `json:"from_item_id"`
	ToID           string `json:"to_item_id"`
	DurationFrames int    `json:"duration_frames"`
}

// TimelineClip is one record-side item. SourceInFrame is the media in-point.
type TimelineClip struct {
	ID                   string             `json:"id"`
	Name                 string             `json:"name"`
	Track                string             `json:"track"`
	Kind                 string             `json:"kind"`
	StartFrame           int                `json:"start_frame"`
	DurationFrames       int                `json:"duration_frames"`
	SourceInFrame        int                `json:"source_in_frame"`
	SourceDurationFrames int                `json:"source_duration_frames,omitempty"`
	MediaPath            string             `json:"media_path,omitempty"`
	MediaType            string             `json:"media_type,omitempty"`
	Color                string             `json:"color,omitempty"`
	WaveSeed             int                `json:"wave_seed,omitempty"`
	LinkID               string             `json:"link_id,omitempty"`
	Enabled              *bool              `json:"enabled,omitempty"`
	Transform            *TimelineTransform `json:"transform,omitempty"`
	Playback             *TimelinePlayback  `json:"playback,omitempty"`
	Audio                *TimelineAudio     `json:"audio,omitempty"`
	Grade                *TimelineColor     `json:"grade,omitempty"`
	Title                *TimelineTitle     `json:"title,omitempty"`
	Captions             *TimelineCaptions  `json:"captions,omitempty"`
	Keyframes            []TimelineKeyframe `json:"keyframes,omitempty"`
}

func emptyTimeline() Timeline {
	return Timeline{
		Schema: timelineSchema,
		FPS:    timelineDefaultFPS,
		Canvas: TimelineCanvas{Width: 1920, Height: 1080},
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
	return s.SaveTimelineCommit(projectID, doc, -1, CommitMeta{})
}

func (s *Store) SaveTimelineCommit(projectID string, doc Timeline, expected int, meta CommitMeta) (Timeline, error) {
	return s.saveTimelineCommit(projectID, doc, expected, meta, false)
}

func (s *Store) CommitMediaState(projectID string, expected int, meta CommitMeta) (Timeline, error) {
	doc, err := s.GetTimeline(projectID)
	if err != nil {
		return Timeline{}, err
	}
	return s.saveTimelineCommit(projectID, doc, expected, meta, true)
}

func (s *Store) CommitTimelineAndMedia(projectID string, doc Timeline, expected int, meta CommitMeta) (Timeline, error) {
	return s.saveTimelineCommit(projectID, doc, expected, meta, true)
}

func (s *Store) saveTimelineCommit(projectID string, doc Timeline, expected int, meta CommitMeta, captureMedia bool) (Timeline, error) {
	p, err := s.Get(projectID)
	if err != nil {
		return Timeline{}, err
	}
	normalized, err := normalizeTimeline(doc)
	if err != nil {
		return Timeline{}, err
	}
	var snapped map[string]string
	if captureMedia {
		prev := map[string]string{}
		if head, headErr := readHead(p); headErr == nil {
			if rev, revErr := readRevision(p, head); revErr == nil {
				prev = rev.Media
			}
		}
		snapped, err = snapshotMedia(p, prev)
		if err != nil {
			return Timeline{}, err
		}
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
	revisions, err := listRevisions(p)
	if err != nil {
		return Timeline{}, err
	}
	nextID := head + 1
	if len(revisions) > 0 && revisions[len(revisions)-1].ID >= nextID {
		nextID = revisions[len(revisions)-1].ID + 1
	}
	normalized.Revision = nextID
	normalized.UpdatedAt = time.Now().UTC()
	if err := writeTimeline(p, normalized); err != nil {
		return Timeline{}, err
	}
	meta = normalizeMeta(meta)
	parent := head
	parentRevision, err := readRevision(p, head)
	if err != nil {
		_ = writeTimeline(p, current)
		return Timeline{}, err
	}
	media := parentRevision.Media
	if captureMedia {
		media = snapped
	}
	rev := Revision{ID: nextID, ParentID: &parent, Actor: meta.Actor, Summary: meta.Summary, ChatID: meta.ChatID, CreatedAt: normalized.UpdatedAt, Timeline: normalized, Media: media}
	if err := writeRevision(p, rev); err != nil {
		_ = writeTimeline(p, current)
		return Timeline{}, err
	}
	if err := writeIntAtomic(headPath(p), nextID); err != nil {
		_ = writeTimeline(p, current)
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
	if doc.Schema == 1 {
		doc.Schema = timelineSchema
	}
	if doc.FPS == 0 {
		doc.FPS = timelineDefaultFPS
	}
	if doc.Canvas.Width == 0 {
		doc.Canvas.Width = 1920
	}
	if doc.Canvas.Height == 0 {
		doc.Canvas.Height = 1080
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
	if doc.Schema == 1 {
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
	if doc.Canvas.Width == 0 {
		doc.Canvas.Width = 1920
	}
	if doc.Canvas.Height == 0 {
		doc.Canvas.Height = 1080
	}
	if doc.Canvas.Width < 16 || doc.Canvas.Width > 16384 || doc.Canvas.Height < 16 || doc.Canvas.Height > 16384 {
		return Timeline{}, fmt.Errorf("%w: invalid canvas size", ErrInvalidTimeline)
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
	if err := normalizeTransitions(&doc); err != nil {
		return Timeline{}, err
	}
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
	if clip.Transform != nil {
		t := clip.Transform
		if t.ScaleX == 0 {
			t.ScaleX = 1
		}
		if t.ScaleY == 0 {
			t.ScaleY = 1
		}
		if t.Opacity == 0 && clip.Enabled == nil {
			t.Opacity = 1
		}
		if t.ScaleX < 0.01 || t.ScaleX > 20 || t.ScaleY < 0.01 || t.ScaleY > 20 || t.Opacity < 0 || t.Opacity > 1 {
			return TimelineClip{}, fmt.Errorf("%w: clip %q has invalid transform", ErrInvalidTimeline, clip.ID)
		}
		for _, crop := range []float64{t.CropTop, t.CropRight, t.CropBottom, t.CropLeft} {
			if crop < 0 || crop >= 1 {
				return TimelineClip{}, fmt.Errorf("%w: clip %q has invalid crop", ErrInvalidTimeline, clip.ID)
			}
		}
		if t.CropTop+t.CropBottom >= 1 || t.CropLeft+t.CropRight >= 1 {
			return TimelineClip{}, fmt.Errorf("%w: clip %q crop removes the frame", ErrInvalidTimeline, clip.ID)
		}
	}
	if clip.Playback != nil {
		if clip.Playback.Rate == 0 {
			clip.Playback.Rate = 1
		}
		if clip.Playback.Rate < 0.1 || clip.Playback.Rate > 8 {
			return TimelineClip{}, fmt.Errorf("%w: clip %q has invalid playback rate", ErrInvalidTimeline, clip.ID)
		}
	}
	if clip.Audio != nil && (clip.Audio.VolumeDB < -60 || clip.Audio.VolumeDB > 12 || clip.Audio.Pan < -1 || clip.Audio.Pan > 1) {
		return TimelineClip{}, fmt.Errorf("%w: clip %q has invalid audio settings", ErrInvalidTimeline, clip.ID)
	}
	if clip.Grade != nil && (clip.Grade.Exposure < -5 || clip.Grade.Exposure > 5 || clip.Grade.Contrast < -1 || clip.Grade.Contrast > 1 || clip.Grade.Saturation < -1 || clip.Grade.Saturation > 3 || clip.Grade.Temperature < -1 || clip.Grade.Temperature > 1 || clip.Grade.Tint < -1 || clip.Grade.Tint > 1) {
		return TimelineClip{}, fmt.Errorf("%w: clip %q has invalid grade", ErrInvalidTimeline, clip.ID)
	}
	if clip.Kind == "title" || clip.Kind == "caption" {
		if clip.Title == nil {
			clip.Title = &TimelineTitle{Text: clip.Name}
		}
		if clip.Transform == nil {
			if clip.Kind == "caption" {
				clip.Transform = &TimelineTransform{X: 960, Y: 1000, AnchorX: .5, AnchorY: 1, ScaleX: 1, ScaleY: 1, Opacity: 1}
			} else {
				clip.Transform = &TimelineTransform{X: 960, Y: 96, AnchorX: .5, ScaleX: 1, ScaleY: 1, Opacity: 1}
			}
		}
		clip.Title.Text = strings.TrimSpace(clip.Title.Text)
		if clip.Title.Text == "" {
			clip.Title.Text = clip.Name
		}
		if clip.Kind == "title" && clip.Title.Text == "" {
			return TimelineClip{}, fmt.Errorf("%w: title %q text is required", ErrInvalidTimeline, clip.ID)
		}
		if clip.Title.FontSize == 0 {
			if clip.Kind == "caption" {
				clip.Title.FontSize = 32
			} else {
				clip.Title.FontSize = 64
			}
		}
		if clip.Title.FontSize < 4 || clip.Title.FontSize > 1000 {
			return TimelineClip{}, fmt.Errorf("%w: title %q has invalid font size", ErrInvalidTimeline, clip.ID)
		}
		if clip.Title.Fill == "" {
			clip.Title.Fill = "#ffffff"
		}
		if !validColor(clip.Title.Fill) {
			return TimelineClip{}, fmt.Errorf("%w: title %q has invalid fill", ErrInvalidTimeline, clip.ID)
		}
		if clip.Kind == "caption" {
			if clip.MediaType == "" {
				clip.MediaType = "subtitle"
			}
			if clip.Color == "" {
				clip.Color = colorCaption
			}
			if clip.Captions != nil {
				clip.Captions.Language = strings.TrimSpace(clip.Captions.Language)
				path, err := sanitizeMediaPath(clip.Captions.Source)
				if err != nil {
					return TimelineClip{}, fmt.Errorf("%w: caption %q %v", ErrInvalidTimeline, clip.ID, err)
				}
				clip.Captions.Source = path
			}
		}
	}
	for i := range clip.Keyframes {
		key := &clip.Keyframes[i]
		if key.Frame < 0 || key.Frame > clip.DurationFrames {
			return TimelineClip{}, fmt.Errorf("%w: clip %q keyframe is outside the clip", ErrInvalidTimeline, clip.ID)
		}
		if !allowedKeyframeProperty(key.Property) {
			return TimelineClip{}, fmt.Errorf("%w: clip %q has unsupported keyframe property", ErrInvalidTimeline, clip.ID)
		}
		switch key.Easing {
		case "", "linear", "ease_in", "ease_out", "ease_in_out":
		default:
			return TimelineClip{}, fmt.Errorf("%w: clip %q has invalid easing", ErrInvalidTimeline, clip.ID)
		}
	}
	if !allowedMediaTypes[clip.MediaType] {
		return TimelineClip{}, fmt.Errorf("%w: clip %q has invalid media type", ErrInvalidTimeline, clip.ID)
	}
	if clip.Color != "" && !validColor(clip.Color) {
		return TimelineClip{}, fmt.Errorf("%w: clip %q has invalid color", ErrInvalidTimeline, clip.ID)
	}
	if clip.WaveSeed < 0 {
		clip.WaveSeed = 0
	}
	if clip.LinkID != "" {
		if err := validateClipID(clip.LinkID); err != nil {
			return TimelineClip{}, fmt.Errorf("%w: clip %q has an invalid link id", ErrInvalidTimeline, clip.ID)
		}
	}
	return clip, nil
}

func allowedKeyframeProperty(value string) bool {
	switch value {
	case "transform.x", "transform.y", "transform.scale_x", "transform.scale_y", "transform.rotation", "transform.opacity",
		"transform.crop_top", "transform.crop_right", "transform.crop_bottom", "transform.crop_left",
		"grade.exposure", "grade.contrast", "grade.saturation", "grade.temperature", "grade.tint",
		"audio.volume_db", "audio.pan", "title.font_size":
		return true
	}
	return false
}

func normalizeTransitions(doc *Timeline) error {
	seen := map[string]bool{}
	items := map[string]TimelineClip{}
	for _, clip := range doc.Clips {
		items[clip.ID] = clip
	}
	for i := range doc.Transitions {
		tr := &doc.Transitions[i]
		if err := validateClipID(tr.ID); err != nil {
			return err
		}
		if seen[tr.ID] {
			return fmt.Errorf("%w: duplicate transition id %q", ErrInvalidTimeline, tr.ID)
		}
		seen[tr.ID] = true
		from, fromOK := items[tr.FromID]
		to, toOK := items[tr.ToID]
		if !fromOK || !toOK || from.Track != to.Track {
			return fmt.Errorf("%w: transition %q references incompatible items", ErrInvalidTimeline, tr.ID)
		}
		switch tr.Type {
		case "crossfade", "dip_black", "dip_white":
		default:
			return fmt.Errorf("%w: transition %q has unsupported type", ErrInvalidTimeline, tr.ID)
		}
		if tr.DurationFrames < 1 || tr.DurationFrames > from.DurationFrames || tr.DurationFrames > to.DurationFrames {
			return fmt.Errorf("%w: transition %q has invalid duration", ErrInvalidTimeline, tr.ID)
		}
	}
	return nil
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
