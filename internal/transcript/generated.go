package transcript

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"parallax/internal/projects"
	"parallax/internal/qdrant"
)

const KindGeneratedAudio = "generated_audio"

type GeneratedAudioMetadata struct {
	GenerationType  string   `json:"generation_type"`
	Prompt          string   `json:"prompt,omitempty"`
	VoiceID         string   `json:"voice_id,omitempty"`
	VoiceName       string   `json:"voice_name,omitempty"`
	Characteristics []string `json:"characteristics,omitempty"`
	Model           string   `json:"model,omitempty"`
	Genres          []string `json:"genres,omitempty"`
	Lyrics          string   `json:"lyrics,omitempty"`
	Description     string   `json:"description,omitempty"`
	SongID          string   `json:"song_id,omitempty"`
}

// IndexGeneratedAudio stores exact TTS alignment/transcript data and embeds
// descriptive metadata for all generated audio without invoking Whisper.
func (x *Indexer) IndexGeneratedAudio(ctx context.Context, projectID, rel string, doc *Document, metadata GeneratedAudioMetadata) error {
	if x == nil || x.Projects == nil {
		return nil
	}
	project, err := x.Projects.Get(projectID)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if rel == "" {
		return fmt.Errorf("generated audio path is required")
	}
	abs, err := x.Projects.ResolveFile(projectID, rel)
	if err != nil {
		return err
	}
	hash, err := projects.HashFile(abs)
	if err != nil {
		return err
	}
	x.Mark(projectID, rel, StateIndexing, "")
	started := time.Now()
	fail := func(err error) error {
		if err != nil {
			x.AddTiming(projectID, rel, TimingIndex, sinceMs(started))
			x.Mark(projectID, rel, StateIndexFailed, err.Error())
		}
		return err
	}
	if doc != nil {
		doc.ContentHash = hash
		doc.Path = rel
		if err := x.ensureEnglish(ctx, doc); err != nil {
			return fail(err)
		}
		if err := Save(project.Dir, doc); err != nil {
			return fail(err)
		}
		if x.canEmbed() {
			if err := x.upsert(ctx, projectID, doc); err != nil {
				return fail(err)
			}
			doc.Embedded = true
			if err := Save(project.Dir, doc); err != nil {
				return fail(err)
			}
		}
	}
	if x.canEmbed() {
		text := generatedSearchText(metadata)
		if strings.TrimSpace(text) != "" {
			vectors, err := x.Embeddings.Embed(ctx, []string{text})
			if err != nil {
				return fail(err)
			}
			if len(vectors) != 1 || len(vectors[0]) == 0 {
				return fail(fmt.Errorf("generated audio embed: no vector returned"))
			}
			collection := qdrant.CollectionName(projectID)
			if err := x.Qdrant.EnsureCollection(ctx, collection, len(vectors[0])); err != nil {
				return fail(err)
			}
			if err := x.Qdrant.DeleteByPathAndKind(ctx, collection, rel, KindGeneratedAudio, false); err != nil {
				return fail(err)
			}
			payload := map[string]any{
				"kind":            KindGeneratedAudio,
				"content_hash":    hash,
				"path":            rel,
				"generation_type": metadata.GenerationType,
				"prompt":          metadata.Prompt,
				"voice_id":        metadata.VoiceID,
				"voice_name":      metadata.VoiceName,
				"characteristics": metadata.Characteristics,
				"model":           metadata.Model,
				"genres":          metadata.Genres,
				"lyrics":          metadata.Lyrics,
				"description":     metadata.Description,
				"song_id":         metadata.SongID,
			}
			if err := x.Qdrant.Upsert(ctx, collection, []qdrant.Point{{
				ID: qdrant.PointID(hash, KindGeneratedAudio), Vector: vectors[0], Payload: payload,
			}}); err != nil {
				return fail(err)
			}
		}
	}
	x.AddTiming(projectID, rel, TimingIndex, sinceMs(started))
	x.Mark(projectID, rel, StateReady, "")
	x.logTimings(projectID, rel)
	return nil
}

func generatedSearchText(metadata GeneratedAudioMetadata) string {
	parts := []string{
		metadata.GenerationType,
		metadata.Prompt,
		metadata.VoiceID,
		metadata.VoiceName,
		strings.Join(metadata.Characteristics, ", "),
		metadata.Model,
		strings.Join(metadata.Genres, ", "),
		metadata.Lyrics,
		metadata.Description,
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}
