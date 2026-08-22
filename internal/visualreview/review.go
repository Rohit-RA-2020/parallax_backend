package visualreview

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"parallax/internal/ffmpeg"
	"parallax/internal/llm"
	"parallax/internal/projects"
)

type Mode string

const (
	ModeChanged Mode = "changed"
	ModeFull    Mode = "full"
)

type Request struct {
	ProjectID  string    `json:"project_id,omitempty"`
	Revision   int       `json:"revision"`
	Mode       Mode      `json:"mode"`
	FocusTimes []float64 `json:"focus_times,omitempty"`
}

type Frame struct {
	ID      string  `json:"id"`
	Path    string  `json:"path"`
	Time    float64 `json:"time"`
	Role    string  `json:"role,omitempty"`
	Width   int     `json:"width,omitempty"`
	Height  int     `json:"height,omitempty"`
	AvgLuma float64 `json:"avg_luma,omitempty"`
}

type Finding struct {
	ID                 string         `json:"id"`
	Time               float64        `json:"time"`
	Type               string         `json:"type"`
	Severity           string         `json:"severity"`
	Confidence         float64        `json:"confidence"`
	Title              string         `json:"title"`
	Detail             string         `json:"detail"`
	FrameIDs           []string       `json:"frame_ids,omitempty"`
	SuggestedOperation map[string]any `json:"suggested_operation,omitempty"`
	Source             string         `json:"source,omitempty"`
}

type Result struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Revision  int       `json:"revision"`
	Mode      Mode      `json:"mode"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Frames    []Frame   `json:"frames,omitempty"`
	Findings  []Finding `json:"findings,omitempty"`
}

type Service struct {
	Store        *projects.Store
	Bins         ffmpeg.Bins
	Vision       llm.ChatProvider
	RenderWidth  int
	RenderHeight int
}

func (s *Service) Review(ctx context.Context, req Request) (Result, error) {
	if s == nil || s.Store == nil {
		return Result{}, errors.New("visual review is unavailable")
	}
	doc, err := s.Store.GetTimeline(req.ProjectID)
	if err != nil {
		return Result{}, err
	}
	if req.Revision <= 0 {
		req.Revision = doc.Revision
	}
	if req.Revision != doc.Revision {
		return Result{}, fmt.Errorf("review revision %d is stale; current revision is %d", req.Revision, doc.Revision)
	}
	var previous *projects.Timeline
	if req.Mode == ModeChanged {
		previous = previousTimeline(s.Store, req.ProjectID, doc.Revision)
	}
	return s.ReviewDocument(ctx, req, doc, previous, true)
}

func (s *Service) ReviewDocument(ctx context.Context, req Request, doc projects.Timeline, previous *projects.Timeline, persist bool) (Result, error) {
	if req.Mode != ModeFull && req.Mode != ModeChanged {
		req.Mode = ModeChanged
	}
	if req.Revision <= 0 {
		req.Revision = doc.Revision
	}
	if req.ProjectID == "" {
		return Result{}, errors.New("project id is required")
	}
	focus := normalizeFocusTimes(req.Mode, req.FocusTimes, doc, previous)
	result := Result{
		ID:        fmt.Sprintf("review-%d-%d", req.Revision, time.Now().UnixNano()),
		ProjectID: req.ProjectID,
		Revision:  req.Revision,
		Mode:      req.Mode,
		Status:    "ready",
		CreatedAt: time.Now().UTC(),
	}
	if len(focus) == 0 {
		result.Status = "degraded"
		result.Error = "timeline has no visual review points"
		if persist {
			_ = s.save(req.ProjectID, result)
		}
		return result, nil
	}

	width, height := s.renderSize(doc)
	root, err := s.Store.ResolveFile(req.ProjectID, "")
	if err != nil {
		// ResolveFile deliberately rejects the project root. Get it through the
		// public project record instead so review artifacts never leave the project.
		project, projectErr := s.Store.Get(req.ProjectID)
		if projectErr != nil {
			return Result{}, projectErr
		}
		root = project.Dir
	}
	artifactDir := filepath.Join(root, ".parallax", "visual-reviews", strconv.Itoa(req.Revision))
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return Result{}, err
	}
	clips := sequenceClips(doc)
	frameGroups := make([][]Frame, 0, len(focus))
	frameIndex := 0
	for _, point := range focus {
		group := make([]Frame, 0, 3)
		for offset, role := range []string{"before", "at", "after"} {
			t := clampTime(point+float64(offset-1)/float64(max(1, doc.FPS)), sequenceEnd(doc))
			name := fmt.Sprintf("frame-%03d-%s.png", frameIndex, role)
			rel := filepath.ToSlash(filepath.Join(".parallax", "visual-reviews", strconv.Itoa(req.Revision), name))
			abs := filepath.Join(root, filepath.FromSlash(rel))
			if err := renderFrame(ctx, s.Bins, root, clips, doc.FPS, t, width, height, abs); err != nil {
				result.Status = "degraded"
				result.Error = err.Error()
				continue
			}
			frame, frameErr := inspectFrame(abs, fmt.Sprintf("frame-%03d-%s", frameIndex, role), rel, t, role)
			if frameErr != nil {
				result.Status = "degraded"
				result.Error = frameErr.Error()
				continue
			}
			result.Frames = append(result.Frames, frame)
			group = append(group, frame)
		}
		if len(group) > 0 {
			frameGroups = append(frameGroups, group)
		}
		frameIndex++
	}

	result.Findings = deterministicFindings(frameGroups)
	if s.Vision != nil {
		for _, group := range frameGroups {
			findings, visionErr := s.visionFindings(ctx, root, group)
			if visionErr != nil {
				result.Status = "degraded"
				result.Error = visionErr.Error()
				continue
			}
			result.Findings = append(result.Findings, findings...)
		}
	} else if result.Status == "ready" {
		result.Status = "degraded"
		result.Error = "vision review is unavailable; deterministic checks completed"
	}
	sort.SliceStable(result.Findings, func(i, j int) bool {
		if result.Findings[i].Time != result.Findings[j].Time {
			return result.Findings[i].Time < result.Findings[j].Time
		}
		return result.Findings[i].Severity < result.Findings[j].Severity
	})
	for i := range result.Findings {
		result.Findings[i].ID = fmt.Sprintf("%s-finding-%d", result.ID, i+1)
	}
	if persist {
		if err := s.save(req.ProjectID, result); err != nil {
			return Result{}, err
		}
	}
	return result, nil
}

func (s *Service) Load(projectID string, revision int) (Result, error) {
	project, err := s.Store.Get(projectID)
	if err != nil {
		return Result{}, err
	}
	b, err := os.ReadFile(filepath.Join(project.Dir, ".parallax", "visual-reviews", fmt.Sprintf("revision-%d.json", revision)))
	if err != nil {
		return Result{}, err
	}
	var result Result
	if err := json.Unmarshal(b, &result); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (s *Service) save(projectID string, result Result) error {
	project, err := s.Store.Get(projectID)
	if err != nil {
		return err
	}
	dir := filepath.Join(project.Dir, ".parallax", "visual-reviews")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".review-*")
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
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, filepath.Join(dir, fmt.Sprintf("revision-%d.json", result.Revision))); err != nil {
		return err
	}
	ok = true
	return nil
}

func previousTimeline(store *projects.Store, projectID string, revision int) *projects.Timeline {
	history, err := store.History(projectID)
	if err != nil {
		return nil
	}
	for _, item := range history.Revisions {
		if item.ID == revision && item.ParentID != nil {
			for _, parent := range history.Revisions {
				if parent.ID == *item.ParentID {
					doc := parent.Timeline
					return &doc
				}
			}
		}
	}
	return nil
}

func normalizeFocusTimes(mode Mode, requested []float64, doc projects.Timeline, previous *projects.Timeline) []float64 {
	if len(requested) > 0 {
		return uniqueTimes(requested, sequenceEnd(doc))
	}
	if mode == ModeChanged && previous != nil {
		return changedFocusTimes(*previous, doc)
	}
	var out []float64
	for _, clip := range doc.Clips {
		if clip.Kind == "video" || clip.Kind == "title" || clip.Kind == "caption" {
			out = append(out, framesToSeconds(clip.StartFrame, doc.FPS), framesToSeconds(clip.StartFrame+clip.DurationFrames, doc.FPS))
		}
	}
	for _, tr := range doc.Transitions {
		if clip := clipByID(doc.Clips, tr.ToID); clip != nil {
			out = append(out, framesToSeconds(clip.StartFrame+tr.DurationFrames/2, doc.FPS))
		}
	}
	return uniqueTimes(out, sequenceEnd(doc))
}

func changedFocusTimes(previous, current projects.Timeline) []float64 {
	old := map[string]string{}
	for _, clip := range previous.Clips {
		b, _ := json.Marshal(clip)
		old[clip.ID] = string(b)
	}
	var out []float64
	for _, clip := range current.Clips {
		b, _ := json.Marshal(clip)
		if old[clip.ID] != string(b) {
			out = append(out, framesToSeconds(clip.StartFrame, current.FPS), framesToSeconds(clip.StartFrame+clip.DurationFrames, current.FPS))
		}
		delete(old, clip.ID)
	}
	for _, clip := range previous.Clips {
		if _, ok := old[clip.ID]; ok {
			out = append(out, framesToSeconds(clip.StartFrame, current.FPS), framesToSeconds(clip.StartFrame+clip.DurationFrames, current.FPS))
		}
	}
	return uniqueTimes(out, sequenceEnd(current))
}

func uniqueTimes(times []float64, end float64) []float64 {
	seen := map[int]bool{}
	out := make([]float64, 0, len(times))
	for _, t := range times {
		if !math.IsNaN(t) && !math.IsInf(t, 0) {
			ms := int(math.Round(clampTime(t, end) * 1000))
			if !seen[ms] {
				seen[ms] = true
				out = append(out, float64(ms)/1000)
			}
		}
	}
	sort.Float64s(out)
	return out
}

func deterministicFindings(groups [][]Frame) []Finding {
	var out []Finding
	for _, group := range groups {
		if len(group) == 0 {
			continue
		}
		at := group[len(group)/2]
		before, after := group[0], group[len(group)-1]
		if at.AvgLuma < 0.045 && before.AvgLuma > 0.12 && after.AvgLuma > 0.12 {
			out = append(out, Finding{Time: at.Time, Type: "black_frame", Severity: "warning", Confidence: .98, Title: "Possible black frame", Detail: "The rendered frame is nearly black while adjacent frames contain visible picture.", FrameIDs: ids(group), Source: "deterministic"})
		}
		if at.AvgLuma > .97 && before.AvgLuma < .88 && after.AvgLuma < .88 {
			out = append(out, Finding{Time: at.Time, Type: "flash_frame", Severity: "warning", Confidence: .96, Title: "Possible flash frame", Detail: "The rendered frame is nearly white while adjacent frames are substantially darker.", FrameIDs: ids(group), Source: "deterministic"})
		}
		jump := math.Abs(after.AvgLuma - before.AvgLuma)
		if jump > .28 {
			out = append(out, Finding{Time: at.Time, Type: "brightness_jump", Severity: "warning", Confidence: math.Min(.99, .65+jump), Title: "Brightness changes abruptly across cut", Detail: fmt.Sprintf("Average luminance changes by %.0f%% across the review point.", jump*100), FrameIDs: ids(group), Source: "deterministic", SuggestedOperation: map[string]any{"type": "update_item", "property": "grade.exposure"}})
		}
	}
	return out
}

func (s *Service) visionFindings(ctx context.Context, root string, group []Frame) ([]Finding, error) {
	if len(group) < 2 {
		return nil, nil
	}
	refs := make([]llm.ImageRef, 0, len(group))
	for _, frame := range group {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(frame.Path)))
		if err != nil {
			return nil, err
		}
		refs = append(refs, llm.ImageRef{Path: frame.Path, Name: frame.ID, MIME: "image/png", Data: base64.StdEncoding.EncodeToString(data)})
	}
	prompt := `Review these consecutive rendered timeline frames. Return only JSON in the form {"findings":[...]}. Identify only visible editorial problems supported by the frames: jump cuts, mismatched eyelines or framing, title/caption collisions, awkward composition, or obvious continuity problems. Do not report normal cuts. Each finding must include type, severity (info|warning|error), confidence (0..1), title, detail, and optional suggested_operation. If there are no problems return {"findings":[]}.`
	stream, err := s.Vision.Stream(ctx, llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: prompt, Images: refs}}, Temperature: llm.Ptr(.1), ReasoningEffort: llm.ThinkingEffortLow})
	if err != nil {
		return nil, err
	}
	var text strings.Builder
	for delta := range stream {
		if delta.Err != nil {
			return nil, delta.Err
		}
		text.WriteString(delta.Content)
	}
	clean := strings.TrimSpace(text.String())
	clean = strings.TrimPrefix(clean, "```json")
	clean = strings.TrimPrefix(clean, "```")
	clean = strings.TrimSuffix(strings.TrimSpace(clean), "```")
	var decoded struct {
		Findings []Finding `json:"findings"`
	}
	if err := json.Unmarshal([]byte(clean), &decoded); err != nil {
		return nil, fmt.Errorf("vision review returned invalid JSON: %w", err)
	}
	frameIDs := ids(group)
	for i := range decoded.Findings {
		if decoded.Findings[i].Time == 0 {
			decoded.Findings[i].Time = group[len(group)/2].Time
		}
		if len(decoded.Findings[i].FrameIDs) == 0 {
			decoded.Findings[i].FrameIDs = frameIDs
		}
		decoded.Findings[i].Source = "vision"
	}
	return decoded.Findings, nil
}

func inspectFrame(abs, id, rel string, t float64, role string) (Frame, error) {
	f, err := os.Open(abs)
	if err != nil {
		return Frame{}, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return Frame{}, err
	}
	return Frame{ID: id, Path: rel, Time: t, Role: role, Width: img.Bounds().Dx(), Height: img.Bounds().Dy(), AvgLuma: averageLuma(img)}, nil
}

func averageLuma(img image.Image) float64 {
	b := img.Bounds()
	stepX := max(1, b.Dx()/96)
	stepY := max(1, b.Dy()/54)
	var sum float64
	var n float64
	for y := b.Min.Y; y < b.Max.Y; y += stepY {
		for x := b.Min.X; x < b.Max.X; x += stepX {
			r, g, b, _ := img.At(x, y).RGBA()
			sum += (.2126*float64(r) + .7152*float64(g) + .0722*float64(b)) / 65535
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / n
}

func renderFrame(ctx context.Context, bins ffmpeg.Bins, workspace string, clips []ffmpeg.SequenceClip, fps int, t float64, width, height int, out string) error {
	if fps < 1 {
		fps = 24
	}
	tmp := filepath.ToSlash(filepath.Join(".parallax", "visual-reviews", fmt.Sprintf("render-%d.mp4", time.Now().UnixNano())))
	spec := ffmpeg.ExportSpec{Source: ffmpeg.SequenceSource, Format: "mp4", Quality: "draft", Resolution: fmt.Sprintf("%dx%d", width, height), FPS: fps, Start: t, Duration: 1 / float64(fps), Audio: false, Captions: "burn"}
	args, err := ffmpeg.BuildSequenceArgs(spec, clips, tmp)
	if err != nil {
		return err
	}
	cmd, err := ffmpeg.Validate(args, ffmpeg.ValidateOpts{Workspace: workspace})
	if err != nil {
		return err
	}
	defer os.Remove(filepath.Join(workspace, filepath.FromSlash(tmp)))
	if _, err := ffmpeg.Run(ctx, bins, cmd, workspace, 2*time.Minute); err != nil {
		return err
	}
	rel, err := filepath.Rel(workspace, out)
	if err != nil {
		return err
	}
	frameCmd, err := ffmpeg.Validate([]string{"ffmpeg", "-y", "-i", tmp, "-frames:v", "1", "-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black", width, height, width, height), "-f", "image2", filepath.ToSlash(rel)}, ffmpeg.ValidateOpts{Workspace: workspace})
	if err != nil {
		return err
	}
	if _, err := ffmpeg.Run(ctx, bins, frameCmd, workspace, 30*time.Second); err != nil {
		return err
	}
	return nil
}

func sequenceClips(doc projects.Timeline) []ffmpeg.SequenceClip {
	fps := max(1, doc.FPS)
	rate := float64(fps)
	fadeIn := map[string]projects.TimelineTransition{}
	fadeOut := map[string]projects.TimelineTransition{}
	for _, tr := range doc.Transitions {
		fadeIn[tr.ToID] = tr
		if tr.Type != "crossfade" {
			fadeOut[tr.FromID] = tr
		}
	}
	out := make([]ffmpeg.SequenceClip, 0, len(doc.Clips))
	for _, clip := range doc.Clips {
		item := ffmpeg.SequenceClip{Track: clip.Track, Kind: clip.Kind, Path: clip.MediaPath, Name: clip.Name, MediaType: clip.MediaType, Start: float64(clip.StartFrame) / rate, Duration: float64(clip.DurationFrames) / rate, SourceIn: float64(clip.SourceInFrame) / rate, CanvasWidth: doc.Canvas.Width, CanvasHeight: doc.Canvas.Height}
		if clip.Transform != nil {
			item.X, item.Y, item.AnchorX, item.AnchorY = clip.Transform.X, clip.Transform.Y, clip.Transform.AnchorX, clip.Transform.AnchorY
			item.Opacity, item.ScaleX, item.ScaleY, item.Rotation = clip.Transform.Opacity, clip.Transform.ScaleX, clip.Transform.ScaleY, clip.Transform.Rotation
			item.CropTop, item.CropRight, item.CropBottom, item.CropLeft = clip.Transform.CropTop, clip.Transform.CropRight, clip.Transform.CropBottom, clip.Transform.CropLeft
		}
		if clip.Title != nil {
			item.TitleText, item.FontSize, item.Fill = clip.Title.Text, clip.Title.FontSize, clip.Title.Fill
		}
		if clip.Kind == "caption" {
			item.SubtitlePath = clip.MediaPath
			if clip.Captions != nil {
				item.CaptionLang = clip.Captions.Language
			}
		}
		if clip.Playback != nil {
			item.PlaybackRate = clip.Playback.Rate
		}
		if clip.Audio != nil {
			item.VolumeDB, item.Muted = clip.Audio.VolumeDB, clip.Audio.Muted
		}
		if clip.Grade != nil {
			item.Exposure, item.Contrast, item.Saturation = clip.Grade.Exposure, clip.Grade.Contrast, clip.Grade.Saturation
		}
		for _, key := range clip.Keyframes {
			if key.Property == "transform.opacity" {
				item.OpacityKeys = append(item.OpacityKeys, ffmpeg.SequenceKeyframe{Frame: key.Frame, Value: key.Value, Easing: key.Easing})
			}
		}
		if tr, ok := fadeIn[clip.ID]; ok {
			item.FadeIn, item.CrossfadeIn = float64(tr.DurationFrames)/rate, tr.Type == "crossfade"
			if tr.Type == "dip_white" {
				item.FadeColor = "white"
			}
		}
		if tr, ok := fadeOut[clip.ID]; ok {
			item.FadeOut = float64(tr.DurationFrames) / rate
			if tr.Type == "dip_white" {
				item.FadeColor = "white"
			}
		}
		out = append(out, item)
	}
	return out
}

func sequenceEnd(doc projects.Timeline) float64 {
	end := 0
	for _, clip := range doc.Clips {
		end = max(end, clip.StartFrame+clip.DurationFrames)
	}
	return float64(end) / float64(max(1, doc.FPS))
}

func framesToSeconds(frames, fps int) float64 { return float64(frames) / float64(max(1, fps)) }
func clampTime(t, end float64) float64 {
	if end <= 0 {
		return 0
	}
	return math.Max(0, math.Min(t, math.Max(0, end-1/24)))
}
func ids(frames []Frame) []string {
	out := make([]string, 0, len(frames))
	for _, frame := range frames {
		out = append(out, frame.ID)
	}
	return out
}
func clipByID(clips []projects.TimelineClip, id string) *projects.TimelineClip {
	for i := range clips {
		if clips[i].ID == id {
			return &clips[i]
		}
	}
	return nil
}
func (s *Service) renderSize(doc projects.Timeline) (int, int) {
	w, h := s.RenderWidth, s.RenderHeight
	if w <= 0 {
		w = 960
	}
	if h <= 0 {
		h = 540
	}
	if doc.Canvas.Width > 0 && doc.Canvas.Height > 0 && w == 960 && h == 540 {
		if doc.Canvas.Width < w {
			w = doc.Canvas.Width
		}
		if doc.Canvas.Height < h {
			h = doc.Canvas.Height
		}
	}
	return w, h
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
