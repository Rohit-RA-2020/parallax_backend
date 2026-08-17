package tools_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"parallax/internal/projects"
	"parallax/internal/qdrant"
	"parallax/internal/tools"
	"parallax/internal/transcript"
)

func TestGetTranscriptReadsSavedDocument(t *testing.T) {
	store, err := projects.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.Create("Talk")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(project.Dir, "media")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "talk.wav")
	if err := os.WriteFile(path, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	hash, err := projects.HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := transcript.Save(project.Dir, &transcript.Document{
		ContentHash: hash,
		Path:        "media/talk.wav",
		Language:    "en",
		Segments:    []transcript.Segment{{ID: "seg-0000", Start: 0, End: 1.5, Text: "Hello", TextEN: "Hello"}},
		Words:       []transcript.Word{{Start: 0, End: 1.5, Text: "Hello"}},
	}); err != nil {
		t.Fatal(err)
	}

	reg := tools.NewRegistry()
	tools.RegisterTranscript(reg, tools.TranscriptEnv{
		Indexer:   &transcript.Indexer{Projects: store},
		ProjectID: project.ID,
	})
	res := reg.Execute(context.Background(), "get_transcript", `{"path":"media/talk.wav"}`)
	if !res.OK {
		t.Fatal(res.Error)
	}
	doc := res.Output.(*transcript.Document)
	if doc.Segments[0].Text != "Hello" {
		t.Fatalf("doc=%+v", doc)
	}
}

func TestGetImageCaptionReadsSavedDocument(t *testing.T) {
	store, err := projects.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.Create("Stills")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(project.Dir, "media")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "still.jpg")
	if err := os.WriteFile(path, []byte("jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	hash, err := projects.HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := transcript.SaveImage(project.Dir, &transcript.ImageCaption{
		ContentHash: hash,
		Path:        "media/still.jpg",
		Name:        "still.jpg",
		TextEN:      "Night alley with magenta neon",
	}); err != nil {
		t.Fatal(err)
	}

	reg := tools.NewRegistry()
	tools.RegisterTranscript(reg, tools.TranscriptEnv{
		Indexer:   &transcript.Indexer{Projects: store},
		ProjectID: project.ID,
	})
	res := reg.Execute(context.Background(), "get_image_caption", `{"path":"media/still.jpg"}`)
	if !res.OK {
		t.Fatal(res.Error)
	}
	doc := res.Output.(*transcript.ImageCaption)
	if doc.TextEN != "Night alley with magenta neon" || doc.Name != "still.jpg" {
		t.Fatalf("doc=%+v", doc)
	}
}

func TestSearchImagesRequiresEmbedder(t *testing.T) {
	reg := tools.NewRegistry()
	tools.RegisterTranscript(reg, tools.TranscriptEnv{
		Indexer: &transcript.Indexer{
			Qdrant: qdrant.NewClient("http://127.0.0.1:6333", ""),
		},
		ProjectID: "x",
	})
	res := reg.Execute(context.Background(), "search_images", `{"query":"neon alley"}`)
	if res.OK {
		t.Fatal("expected missing embedder error")
	}
}

func TestSearchTranscriptRequiresQuery(t *testing.T) {
	reg := tools.NewRegistry()
	tools.RegisterTranscript(reg, tools.TranscriptEnv{
		Indexer: &transcript.Indexer{
			Embeddings: nil,
			Qdrant:     qdrant.NewClient("http://127.0.0.1:6333", ""),
		},
		ProjectID: "x",
	})
	res := reg.Execute(context.Background(), "search_transcript", `{"query":"thanks"}`)
	if res.OK {
		t.Fatal("expected missing embedder error")
	}
}

func TestAddCaptionsPlacesVisibleTimelineTrack(t *testing.T) {
	store, err := projects.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.Create("Caps")
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(project.Dir, "media", "talk.mp4")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("not-a-real-video"), 0o644); err != nil {
		t.Fatal(err)
	}
	hash, err := projects.HashFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := transcript.Save(project.Dir, &transcript.Document{
		ContentHash: hash,
		Path:        "media/talk.mp4",
		Language:    "en",
		Segments:    []transcript.Segment{{Start: 0.1, End: 0.8, Text: "Hello there", TextEN: "Hello there"}},
	}); err != nil {
		t.Fatal(err)
	}
	tx, err := store.BeginTimelineTransaction(project.ID, projects.CommitMeta{Actor: "agent", Summary: "Add captions"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Apply([]projects.TimelineOperation{{
		Type: "add_item",
		Item: &projects.TimelineClip{
			Name: "talk", Track: "V1", Kind: "video",
			StartFrame: 0, DurationFrames: 24, SourceDurationFrames: 24,
			MediaPath: "media/talk.mp4", MediaType: "video",
		},
	}}); err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry()
	tools.RegisterTranscript(reg, tools.TranscriptEnv{
		Indexer:     &transcript.Indexer{Projects: store},
		ProjectID:   project.ID,
		Workspace:   project.Dir,
		Transaction: tx,
	})
	res := reg.Execute(context.Background(), "add_captions", `{"path":"media/talk.mp4","language":"en","style":"soft"}`)
	if !res.OK {
		t.Fatal(res.Error)
	}
	if _, err := os.Stat(filepath.Join(project.Dir, "media", "talk.en.srt")); !os.IsNotExist(err) {
		t.Fatal("soft captions must not drop an SRT into the media bin")
	}
	matches, _ := filepath.Glob(filepath.Join(project.Dir, ".parallax", "captions", "*.srt"))
	if len(matches) != 1 {
		t.Fatalf("expected one hidden caption file, got %v", matches)
	}
	doc := tx.Get()
	var captions []projects.TimelineClip
	for _, clip := range doc.Clips {
		if clip.Kind == "caption" {
			captions = append(captions, clip)
		}
	}
	if len(captions) != 1 || captions[0].Track != "C1" || captions[0].MediaType != "subtitle" {
		t.Fatalf("captions=%+v", captions)
	}
	if captions[0].Captions == nil || captions[0].Captions.Source != "media/talk.mp4" {
		t.Fatalf("caption meta=%+v", captions[0].Captions)
	}
	out, ok := res.Output.(map[string]any)
	if !ok || out["visible"] != true || out["track"] != "C1" {
		t.Fatalf("output=%#v", res.Output)
	}
}

func TestAddCaptionsReplacesPreviousTrack(t *testing.T) {
	store, err := projects.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.Create("Caps")
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(project.Dir, "media", "talk.mp4")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	hash, err := projects.HashFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := transcript.Save(project.Dir, &transcript.Document{
		ContentHash: hash,
		Path:        "media/talk.mp4",
		Language:    "en",
		Segments:    []transcript.Segment{{Start: 0, End: 1, Text: "Hello", TextEN: "Hello"}},
	}); err != nil {
		t.Fatal(err)
	}
	tx, err := store.BeginTimelineTransaction(project.ID, projects.CommitMeta{Actor: "agent", Summary: "Add captions"})
	if err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry()
	tools.RegisterTranscript(reg, tools.TranscriptEnv{
		Indexer:     &transcript.Indexer{Projects: store},
		ProjectID:   project.ID,
		Workspace:   project.Dir,
		Transaction: tx,
	})
	first := reg.Execute(context.Background(), "add_captions", `{"path":"media/talk.mp4","language":"en"}`)
	if !first.OK {
		t.Fatal(first.Error)
	}
	second := reg.Execute(context.Background(), "add_captions", `{"path":"media/talk.mp4","language":"en"}`)
	if !second.OK {
		t.Fatal(second.Error)
	}
	count := 0
	for _, clip := range tx.Get().Clips {
		if clip.Kind == "caption" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("caption clips=%d", count)
	}
}
