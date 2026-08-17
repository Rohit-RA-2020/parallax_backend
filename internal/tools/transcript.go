package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"parallax/internal/ffmpeg"
	"parallax/internal/llm"
	"parallax/internal/projects"
	"parallax/internal/transcript"
)

// TranscriptEnv is the Director search/read surface for imported speech.
type TranscriptEnv struct {
	Indexer     *transcript.Indexer
	ProjectID   string
	Workspace   string
	Bins        ffmpeg.Bins
	Transaction *projects.TimelineTransaction
	OnMutation  func()
	OnApplied   func(rel string)
}

func RegisterTranscript(reg *Registry, env TranscriptEnv) {
	reg.Register(llm.NewFunctionTool(
		"search_transcript",
		"Semantic search over this project's transcribed speech. Query in English only — non-English speech was translated before indexing. Optionally limit to one or more workspace paths. Returns original text, English text, file path, and start/end seconds.",
		json.RawMessage(`{
			"type":"object",
			"properties":{
				"query":{"type":"string","description":"English search query, e.g. when they thank the guest"},
				"paths":{"type":"array","items":{"type":"string"},"description":"Optional workspace-relative media paths to search inside"},
				"path":{"type":"string","description":"Optional single file filter; same as paths with one entry"},
				"limit":{"type":"integer","minimum":1,"maximum":50,"description":"Maximum hits, default 8"}
			},
			"required":["query"]
		}`),
	), env.searchTranscript)

	reg.Register(llm.NewFunctionTool(
		"get_transcript",
		"Read the timed transcript for one workspace media file. Includes original-language words/segments and English segment translations when available.",
		json.RawMessage(`{
			"type":"object",
			"properties":{
				"path":{"type":"string","description":"Workspace-relative media path such as media/talk.mp4"}
			},
			"required":["path"]
		}`),
	), env.getTranscript)
	env.registerCaptions(reg)
}

func (e TranscriptEnv) searchTranscript(ctx context.Context, raw json.RawMessage) Result {
	if e.Indexer == nil {
		return Result{OK: false, Error: "transcript search is not configured"}
	}
	var in struct {
		Query string   `json:"query"`
		Path  string   `json:"path"`
		Paths []string `json:"paths"`
		Limit int      `json:"limit"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	paths := append([]string{}, in.Paths...)
	if strings.TrimSpace(in.Path) != "" {
		paths = append(paths, in.Path)
	}
	hits, err := e.Indexer.Search(ctx, e.ProjectID, in.Query, paths, in.Limit)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	results := make([]map[string]any, 0, len(hits))
	for _, hit := range hits {
		item := map[string]any{"score": hit.Score}
		for _, key := range []string{"path", "start", "end", "text", "text_en", "language", "segment_id"} {
			if v, ok := hit.Payload[key]; ok {
				item[key] = v
			}
		}
		results = append(results, item)
	}
	return Result{OK: true, Output: map[string]any{
		"query":   strings.TrimSpace(in.Query),
		"count":   len(results),
		"results": results,
		"note":    "Queries must be English. start/end are seconds in the source file.",
	}}
}

func (e TranscriptEnv) getTranscript(_ context.Context, raw json.RawMessage) Result {
	if e.Indexer == nil {
		return Result{OK: false, Error: "transcripts are not configured"}
	}
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	path := filepath.ToSlash(strings.TrimSpace(in.Path))
	if path == "" {
		return Result{OK: false, Error: "path is required"}
	}
	doc, err := e.Indexer.Get(e.ProjectID, path)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	return Result{OK: true, Output: doc}
}
