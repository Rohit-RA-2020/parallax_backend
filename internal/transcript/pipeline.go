package transcript

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"parallax/internal/embed"
	"parallax/internal/ffmpeg"
	"parallax/internal/llm"
	"parallax/internal/projects"
	"parallax/internal/qdrant"
)

// Indexer transcribes imported media, translates segments, and upserts vectors.
type Indexer struct {
	Projects   *projects.Store
	Bins       ffmpeg.Bins
	Whisper    Transcriber
	Embeddings *embed.Client
	Qdrant     *qdrant.Client
	Completer  func() llm.Completer
	Logger     *slog.Logger

	mu   sync.Mutex
	live map[string]JobStatus
}

func (x *Indexer) log() *slog.Logger {
	if x != nil && x.Logger != nil {
		return x.Logger
	}
	return slog.Default()
}

// Enabled is true when ASR is configured. Embeddings may still be skipped.
func (x *Indexer) Enabled() bool {
	return x != nil && x.Projects != nil && x.Whisper != nil
}

// Index transcribes and embeds one project-relative media file.
func (x *Indexer) Index(ctx context.Context, projectID, rel string) error {
	if !x.Enabled() {
		return nil
	}
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if rel == "" {
		return fmt.Errorf("media path is required")
	}
	project, err := x.Projects.Get(projectID)
	if err != nil {
		return err
	}
	abs, err := x.Projects.ResolveFile(projectID, rel)
	if err != nil {
		return err
	}
	if !hasAudioExt(rel) {
		x.Mark(projectID, rel, StateSkipped, "")
		return nil
	}
	info, err := ffmpeg.ProbeMedia(ctx, x.Bins, project.Dir, rel)
	if err != nil {
		return err
	}
	if !info.HasAudio {
		x.Mark(projectID, rel, StateSkipped, "")
		return nil
	}
	hash, err := projects.HashFile(abs)
	if err != nil {
		return err
	}

	doc, err := Load(project.Dir, hash)
	if err != nil {
		return err
	}
	if doc == nil || len(doc.Segments) == 0 {
		x.Mark(projectID, rel, StateTranscribing, "")
		doc, err = x.transcribe(ctx, project.Dir, rel, hash, info.Duration)
		if err != nil {
			return err
		}
	} else {
		doc.Path = rel
	}

	if needsEnglish(doc.Segments) {
		x.Mark(projectID, rel, StateTranslating, "")
	}
	if err := x.ensureEnglish(ctx, doc); err != nil {
		_ = Save(project.Dir, doc)
		return err
	}
	if err := Save(project.Dir, doc); err != nil {
		return err
	}
	if x.Embeddings != nil && x.Qdrant != nil {
		x.Mark(projectID, rel, StateIndexing, "")
	}
	if err := x.upsert(ctx, projectID, doc); err != nil {
		return err
	}
	x.Mark(projectID, rel, StateReady, "")
	return nil
}

func needsEnglish(segments []Segment) bool {
	for _, seg := range segments {
		if strings.TrimSpace(seg.Text) != "" && strings.TrimSpace(seg.TextEN) == "" {
			return true
		}
	}
	return false
}

func (x *Indexer) transcribe(ctx context.Context, projectDir, rel, hash string, duration float64) (*Document, error) {
	scratch := filepath.ToSlash(filepath.Join(".scratch", "asr-"+hash+".wav"))
	if err := ffmpeg.ExtractMono16k(ctx, x.Bins, projectDir, rel, scratch); err != nil {
		return nil, err
	}
	defer os.Remove(filepath.Join(projectDir, filepath.FromSlash(scratch)))

	asr, err := x.Whisper.Transcribe(ctx, filepath.Join(projectDir, filepath.FromSlash(scratch)))
	if err != nil {
		return nil, err
	}
	assignSegmentIDs(asr.Segments)
	doc := &Document{
		ContentHash: hash,
		Path:        rel,
		Language:    asr.Language,
		Duration:    duration,
		ASRModel:    asr.Model,
		Words:       asr.Words,
		Segments:    asr.Segments,
	}
	if err := Save(projectDir, doc); err != nil {
		return nil, err
	}
	return doc, nil
}

func (x *Indexer) ensureEnglish(ctx context.Context, doc *Document) error {
	missing := false
	for i := range doc.Segments {
		if strings.TrimSpace(doc.Segments[i].Text) != "" && strings.TrimSpace(doc.Segments[i].TextEN) == "" {
			missing = true
			break
		}
	}
	if !missing {
		return nil
	}
	if x.Completer == nil {
		return fmt.Errorf("transcript translator is not configured")
	}
	completer := x.Completer()
	if completer == nil {
		return fmt.Errorf("transcript translator is not configured")
	}
	return TranslateSegments(ctx, completer, doc.Language, doc.Segments)
}

func (x *Indexer) upsert(ctx context.Context, projectID string, doc *Document) error {
	if x.Embeddings == nil || x.Qdrant == nil {
		x.log().Info("skip transcript embed", "reason", "embeddings or qdrant not configured", "path", doc.Path)
		return nil
	}
	var texts []string
	var segs []Segment
	for i, seg := range doc.Segments {
		window := NeighborWindow(doc.Segments, i)
		if strings.TrimSpace(window) == "" {
			continue
		}
		texts = append(texts, window)
		segs = append(segs, seg)
	}
	if len(texts) == 0 {
		return nil
	}
	vectors, err := x.Embeddings.Embed(ctx, texts)
	if err != nil {
		return err
	}
	if len(vectors) == 0 {
		return fmt.Errorf("embed: no vectors returned")
	}
	collection := qdrant.CollectionName(projectID)
	if err := x.Qdrant.EnsureCollection(ctx, collection, len(vectors[0])); err != nil {
		return err
	}
	if err := x.Qdrant.DeleteByPath(ctx, collection, doc.Path); err != nil {
		return err
	}
	points := make([]qdrant.Point, 0, len(segs))
	for i, seg := range segs {
		points = append(points, qdrant.Point{
			ID:     qdrant.PointID(doc.ContentHash, seg.ID),
			Vector: vectors[i],
			Payload: map[string]any{
				"content_hash": doc.ContentHash,
				"path":         doc.Path,
				"start":        seg.Start,
				"end":          seg.End,
				"text":         seg.Text,
				"text_en":      seg.TextEN,
				"language":     doc.Language,
				"segment_id":   seg.ID,
			},
		})
	}
	return x.Qdrant.Upsert(ctx, collection, points)
}

// RemovePath drops Qdrant points for a deleted or replaced file.
func (x *Indexer) RemovePath(ctx context.Context, projectID, rel string) error {
	if x == nil {
		return nil
	}
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if rel == "" {
		return nil
	}
	x.Clear(projectID, rel)
	if x.Qdrant == nil {
		return nil
	}
	return x.Qdrant.DeleteByPath(ctx, qdrant.CollectionName(projectID), rel)
}

// Get loads the transcript for the current bytes of a project file.
func (x *Indexer) Get(projectID, rel string) (*Document, error) {
	if x == nil || x.Projects == nil {
		return nil, fmt.Errorf("transcripts are not configured")
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
	doc, err := Load(project.Dir, hash)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("no transcript for %s", filepath.ToSlash(rel))
	}
	return doc, nil
}

// Search embeds an English query and returns matching segments.
func (x *Indexer) Search(ctx context.Context, projectID, query string, paths []string, limit int) ([]qdrant.Hit, error) {
	if x == nil || x.Embeddings == nil || x.Qdrant == nil {
		return nil, fmt.Errorf("transcript search is not configured")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	vecs, err := x.Embeddings.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("embed: empty query vector")
	}
	return x.Qdrant.Search(ctx, qdrant.CollectionName(projectID), vecs[0], paths, limit)
}

func assignSegmentIDs(segments []Segment) {
	for i := range segments {
		if strings.TrimSpace(segments[i].ID) == "" {
			segments[i].ID = fmt.Sprintf("seg-%04d", i)
		}
	}
}

// HasSpeech is true for video/audio extensions that may contain a soundtrack.
func HasSpeech(rel string) bool {
	return hasAudioExt(rel)
}

func hasAudioExt(rel string) bool {
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".mp4", ".mov", ".mkv", ".webm", ".avi", ".m4v", ".ts", ".mts",
		".mp3", ".wav", ".aac", ".flac", ".m4a", ".ogg", ".opus":
		return true
	default:
		return false
	}
}
