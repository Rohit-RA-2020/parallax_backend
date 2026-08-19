package transcript

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"parallax/internal/ffmpeg"
	"parallax/internal/llm"
	"parallax/internal/projects"
	"parallax/internal/qdrant"
)

const KindVideoScene = "video_scene"

// HasVideo is true for container extensions that may carry a picture stream.
func HasVideo(rel string) bool {
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".mp4", ".mov", ".mkv", ".webm", ".avi", ".m4v", ".ts", ".mts":
		return true
	default:
		return false
	}
}

func videoScenesComplete(doc *VideoScenes) bool {
	if doc == nil || len(doc.Scenes) == 0 {
		return false
	}
	for _, scene := range doc.Scenes {
		if strings.TrimSpace(scene.TextEN) == "" {
			return false
		}
	}
	return true
}

func (x *Indexer) indexScenes(ctx context.Context, projectID, rel string) error {
	if !x.canCaption() || !HasVideo(rel) {
		return nil
	}
	project, err := x.Projects.Get(projectID)
	if err != nil {
		return err
	}
	abs, err := x.Projects.ResolveFile(projectID, rel)
	if err != nil {
		return err
	}
	info, err := ffmpeg.ProbeMedia(ctx, x.Bins, project.Dir, rel)
	if err != nil {
		return err
	}
	if !info.HasVideo {
		return nil
	}
	hash, err := projects.HashFile(abs)
	if err != nil {
		return err
	}
	name := filepath.Base(rel)
	doc, err := LoadVideoScenes(project.Dir, hash)
	if err != nil {
		return err
	}
	transcript := loadTranscriptIfPresent(project.Dir, hash)

	if videoScenesComplete(doc) {
		doc.Path = rel
		doc.Name = name
		doc.Duration = info.Duration
		attachSpoken(doc, transcript)
		if err := SaveVideoScenes(project.Dir, doc); err != nil {
			return err
		}
		return x.finishVideoScenes(ctx, projectID, doc)
	}

	x.Mark(projectID, rel, StateDescribing, "")
	describeStarted := time.Now()
	cuts, err := ffmpeg.DetectScenes(ctx, x.Bins, project.Dir, rel, ffmpeg.DefaultSceneThreshold)
	if err != nil {
		x.log().Info("scene detect fallback to interval samples", "path", rel, "err", err)
		cuts = nil
	}
	windows := PlanSceneWindows(cuts, info.Duration)
	scenes := mergePlannedScenes(windows, doc)
	doc = &VideoScenes{
		ContentHash: hash,
		Path:        rel,
		Name:        name,
		Duration:    info.Duration,
		Scenes:      scenes,
	}
	attachSpoken(doc, transcript)
	if err := SaveVideoScenes(project.Dir, doc); err != nil {
		return err
	}

	completer := x.Completer()
	if completer == nil {
		return fmt.Errorf("video scene captioner is not configured")
	}
	scratchDir := filepath.ToSlash(filepath.Join(".scratch", "scenes-"+hash))
	defer os.RemoveAll(filepath.Join(project.Dir, filepath.FromSlash(scratchDir)))

	var captioned int
	for i := range doc.Scenes {
		if strings.TrimSpace(doc.Scenes[i].TextEN) != "" {
			captioned++
			continue
		}
		x.MarkCaptionProgress(projectID, rel, i+1, len(doc.Scenes))
		frameRel := filepath.ToSlash(filepath.Join(scratchDir, fmt.Sprintf("%s.jpg", doc.Scenes[i].ID)))
		if err := ffmpeg.ExtractFrame(ctx, x.Bins, project.Dir, rel, frameRel, doc.Scenes[i].At); err != nil {
			x.log().Error("extract scene frame", "path", rel, "at", doc.Scenes[i].At, "err", err)
			continue
		}
		data, err := os.ReadFile(filepath.Join(project.Dir, filepath.FromSlash(frameRel)))
		_ = os.Remove(filepath.Join(project.Dir, filepath.FromSlash(frameRel)))
		if err != nil || !llm.LooksLikeImage(data) {
			continue
		}
		captionCtx, cancel := context.WithTimeout(ctx, imageCaptionTO)
		text, capErr := CaptionVideoFrame(captionCtx, completer, llm.ImageRef{
			Path: frameRel,
			MIME: llm.DetectImageMIME(data),
			Name: name,
			Data: base64.StdEncoding.EncodeToString(data),
		}, name, rel, doc.Scenes[i])
		cancel()
		if capErr != nil {
			x.log().Error("caption scene", "path", rel, "id", doc.Scenes[i].ID, "err", capErr)
			continue
		}
		doc.Scenes[i].TextEN = text
		captioned++
		if err := SaveVideoScenes(project.Dir, doc); err != nil {
			return err
		}
	}
	if captioned == 0 {
		return fmt.Errorf("could not describe any scenes in %s", rel)
	}
	x.AddTiming(projectID, rel, TimingDescribe, sinceMs(describeStarted))
	return x.finishVideoScenes(ctx, projectID, doc)
}

func (x *Indexer) finishVideoScenes(ctx context.Context, projectID string, doc *VideoScenes) error {
	project, err := x.Projects.Get(projectID)
	if err != nil {
		return err
	}
	if !x.canEmbed() {
		x.Mark(projectID, doc.Path, StateReady, "")
		x.logTimings(projectID, doc.Path)
		return nil
	}
	x.Mark(projectID, doc.Path, StateIndexing, "")
	started := time.Now()
	if err := x.upsertVideoScenes(ctx, projectID, doc); err != nil {
		doc.Embedded = false
		_ = SaveVideoScenes(project.Dir, doc)
		x.AddTiming(projectID, doc.Path, TimingIndex, sinceMs(started))
		x.Mark(projectID, doc.Path, StateIndexFailed, err.Error())
		x.log().Error("video scene embed", "project", projectID, "path", doc.Path, "err", err)
		return nil
	}
	x.AddTiming(projectID, doc.Path, TimingIndex, sinceMs(started))
	doc.Embedded = true
	if err := SaveVideoScenes(project.Dir, doc); err != nil {
		return err
	}
	x.Mark(projectID, doc.Path, StateReady, "")
	x.logTimings(projectID, doc.Path)
	return nil
}

func (x *Indexer) upsertVideoScenes(ctx context.Context, projectID string, doc *VideoScenes) error {
	if x.Embeddings == nil || x.Qdrant == nil {
		x.log().Info("skip video scene embed", "reason", "embeddings or qdrant not configured", "path", doc.Path)
		return nil
	}
	var texts []string
	var scenes []VideoScene
	for _, scene := range doc.Scenes {
		text := videoSceneEmbedText(doc, scene)
		if strings.TrimSpace(text) == "" {
			continue
		}
		texts = append(texts, text)
		scenes = append(scenes, scene)
	}
	if len(texts) == 0 {
		return nil
	}
	vectors, err := x.Embeddings.Embed(ctx, texts)
	if err != nil {
		return err
	}
	if len(vectors) == 0 || len(vectors[0]) == 0 {
		return fmt.Errorf("embed: no vectors returned")
	}
	collection := qdrant.CollectionName(projectID)
	if err := x.Qdrant.EnsureCollection(ctx, collection, len(vectors[0])); err != nil {
		return err
	}
	if err := x.Qdrant.DeleteByPathAndKind(ctx, collection, doc.Path, KindVideoScene, false); err != nil {
		return err
	}
	points := make([]qdrant.Point, 0, len(scenes))
	for i, scene := range scenes {
		payload := map[string]any{
			"kind":         KindVideoScene,
			"path":         doc.Path,
			"name":         doc.Name,
			"content_hash": doc.ContentHash,
			"start":        scene.Start,
			"end":          scene.End,
			"at":           scene.At,
			"text_en":      scene.TextEN,
			"scene_id":     scene.ID,
		}
		if strings.TrimSpace(scene.SpokenEN) != "" {
			payload["spoken_en"] = scene.SpokenEN
		}
		points = append(points, qdrant.Point{
			ID:      qdrant.PointID(doc.ContentHash, scene.ID),
			Vector:  vectors[i],
			Payload: payload,
		})
	}
	return x.Qdrant.Upsert(ctx, collection, points)
}

func videoSceneEmbedText(doc *VideoScenes, scene VideoScene) string {
	var b strings.Builder
	if doc != nil {
		if name := strings.TrimSpace(doc.Name); name != "" {
			fmt.Fprintf(&b, "Name: %s\n", name)
		}
		if path := strings.TrimSpace(doc.Path); path != "" {
			fmt.Fprintf(&b, "Path: %s\n", path)
		}
	}
	fmt.Fprintf(&b, "Time: %.1f-%.1fs\n", scene.Start, scene.End)
	if text := strings.TrimSpace(scene.TextEN); text != "" {
		fmt.Fprintf(&b, "Description: %s\n", text)
	}
	if spoken := strings.TrimSpace(scene.SpokenEN); spoken != "" {
		fmt.Fprintf(&b, "Spoken: %s", spoken)
	}
	return strings.TrimSpace(b.String())
}

func mergePlannedScenes(windows []SceneWindow, prev *VideoScenes) []VideoScene {
	var old []VideoScene
	if prev != nil {
		old = prev.Scenes
	}
	out := make([]VideoScene, 0, len(windows))
	for i, win := range windows {
		scene := VideoScene{
			ID:    fmt.Sprintf("scn-%04d", i),
			Start: win.Start,
			End:   win.End,
			At:    win.At,
		}
		for _, prevScene := range old {
			if absFloat(prevScene.Start-win.Start) <= 0.15 && absFloat(prevScene.End-win.End) <= 0.2 {
				scene.TextEN = prevScene.TextEN
				scene.SpokenEN = prevScene.SpokenEN
				break
			}
		}
		out = append(out, scene)
	}
	return out
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func attachSpoken(doc *VideoScenes, transcript *Document) {
	if doc == nil {
		return
	}
	var segments []Segment
	if transcript != nil {
		segments = transcript.Segments
	}
	for i := range doc.Scenes {
		doc.Scenes[i].SpokenEN = spokenInRange(segments, doc.Scenes[i].Start, doc.Scenes[i].End)
	}
}

func loadTranscriptIfPresent(projectDir, hash string) *Document {
	doc, err := Load(projectDir, hash)
	if err != nil {
		return nil
	}
	return doc
}

// CaptionVideoFrame asks a vision-capable completer to describe one shot.
func CaptionVideoFrame(ctx context.Context, completer llm.Completer, image llm.ImageRef, name, path string, scene VideoScene) (string, error) {
	if completer == nil {
		return "", fmt.Errorf("video scene captioner is not configured")
	}
	if strings.TrimSpace(image.Data) == "" {
		return "", fmt.Errorf("image bytes are required")
	}
	var user strings.Builder
	user.WriteString("Describe this video frame so someone could find the shot later by what it looks like.")
	if name = strings.TrimSpace(name); name != "" {
		fmt.Fprintf(&user, "\nFilename: %s", name)
	}
	if path = strings.TrimSpace(path); path != "" {
		fmt.Fprintf(&user, "\nPath: %s", path)
	}
	fmt.Fprintf(&user, "\nShot time: %.1f-%.1fs (frame at %.1fs).", scene.Start, scene.End, scene.At)
	if spoken := strings.TrimSpace(scene.SpokenEN); spoken != "" {
		fmt.Fprintf(&user, "\nSpoken words in this window (do not let them replace the visual description): %s", spoken)
	}
	temp := llm.Ptr(0.2)
	raw, err := completer.Complete(ctx, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: captionSystem},
			{Role: llm.RoleUser, Content: user.String(), Images: []llm.ImageRef{image}},
		},
		Temperature: temp,
	})
	if err != nil {
		return "", err
	}
	text := cleanCaption(raw)
	if text == "" {
		return "", fmt.Errorf("video scene captioner returned an empty description")
	}
	return text, nil
}

// GetVideoScenes loads the scene index for the current bytes of a project video.
func (x *Indexer) GetVideoScenes(projectID, rel string) (*VideoScenes, error) {
	if x == nil || x.Projects == nil {
		return nil, fmt.Errorf("video scenes are not configured")
	}
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if rel == "" {
		return nil, fmt.Errorf("path is required")
	}
	project, err := x.Projects.Get(projectID)
	if err != nil {
		return nil, err
	}
	abs, err := x.Projects.ResolveFile(projectID, rel)
	if err != nil {
		return nil, err
	}
	hash, err := projects.HashFile(abs)
	if err != nil {
		return nil, err
	}
	doc, err := LoadVideoScenes(project.Dir, hash)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("no video scenes for %s", rel)
	}
	return doc, nil
}

// SearchScenes embeds an English query and returns matching video shots.
func (x *Indexer) SearchScenes(ctx context.Context, projectID, query string, paths []string, limit int) ([]qdrant.Hit, error) {
	return x.search(ctx, projectID, query, paths, KindVideoScene, nil, limit)
}
