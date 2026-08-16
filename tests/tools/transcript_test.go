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
