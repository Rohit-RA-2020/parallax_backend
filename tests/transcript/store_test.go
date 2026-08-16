package transcript_test

import (
	"os"
	"path/filepath"
	"testing"

	. "parallax/internal/transcript"
)

func TestSaveLoadAndNeighborWindow(t *testing.T) {
	dir := t.TempDir()
	doc := &Document{
		ContentHash: "abc123",
		Path:        "media/talk.mp4",
		Language:    "hi",
		Segments: []Segment{
			{ID: "seg-0000", Start: 0, End: 2, Text: "नमस्ते", TextEN: "Hello"},
			{ID: "seg-0001", Start: 2, End: 4, Text: "धन्यवाद", TextEN: "Thanks"},
			{ID: "seg-0002", Start: 4, End: 6, Text: "आओ", TextEN: "Come in"},
		},
	}
	if err := Save(dir, doc); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".parallax", "transcripts", "abc123.json")); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir, "abc123")
	if err != nil || got == nil {
		t.Fatalf("load: %+v %v", got, err)
	}
	if got.Path != "media/talk.mp4" || got.Segments[1].TextEN != "Thanks" {
		t.Fatalf("doc=%+v", got)
	}
	if NeighborWindow(got.Segments, 1) != "Hello Thanks Come in" {
		t.Fatalf("window=%q", NeighborWindow(got.Segments, 1))
	}
	if NeighborWindow(got.Segments, 0) != "Hello Thanks" {
		t.Fatalf("first window=%q", NeighborWindow(got.Segments, 0))
	}
	missing, err := Load(dir, "nope")
	if err != nil || missing != nil {
		t.Fatalf("missing=%+v err=%v", missing, err)
	}
}
