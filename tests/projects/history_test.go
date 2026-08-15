package projects_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"parallax/internal/projects"
)

func TestHistoryBranchesAndRestores(t *testing.T) {
	store, err := projects.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.Create("History")
	if err != nil {
		t.Fatal(err)
	}

	doc := projects.Timeline{Schema: 2, FPS: 24, Clips: []projects.TimelineClip{{ID: "a", Name: "A", Track: "V1", Kind: "video", DurationFrames: 24}}}
	one, err := store.SaveTimelineCommit(project.ID, doc, 0, projects.CommitMeta{Summary: "Add A"})
	if err != nil {
		t.Fatal(err)
	}
	doc = one
	doc.Clips[0].StartFrame = 12
	two, err := store.SaveTimelineCommit(project.ID, doc, 1, projects.CommitMeta{Summary: "Move A"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Undo(project.ID, two.Revision); err != nil {
		t.Fatal(err)
	}

	doc = one
	doc.Clips[0].Name = "Alternate"
	three, err := store.SaveTimelineCommit(project.ID, doc, one.Revision, projects.CommitMeta{Summary: "Rename A"})
	if err != nil {
		t.Fatal(err)
	}
	if three.Revision != 3 {
		t.Fatalf("revision=%d", three.Revision)
	}
	history, err := store.History(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	var children []int
	for _, rev := range history.Revisions {
		if rev.ID == 1 {
			children = rev.Children
		}
	}
	if len(children) != 2 || children[0] != 2 || children[1] != 3 {
		t.Fatalf("children=%v", children)
	}
	restored, err := store.RestoreRevision(project.ID, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Clips[0].StartFrame != 12 {
		t.Fatalf("restored=%+v", restored.Clips[0])
	}
}

func TestMediaVersionRestoresExactBytes(t *testing.T) {
	store, err := projects.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.Create("Media history")
	if err != nil {
		t.Fatal(err)
	}
	original := []byte("original-video-bytes")
	media, err := store.SaveUpload(project.ID, "clip.mp4", bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.History(project.ID); err != nil {
		t.Fatal(err)
	}

	tx, err := store.BeginTimelineTransaction(project.ID, projects.CommitMeta{Actor: "agent", Summary: "Versioned transform"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project.Dir, filepath.FromSlash(media.Path))
	if err := os.WriteFile(path, []byte("changed-video-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	tx.MarkMediaMutation()
	doc, changed, err := tx.Commit()
	if err != nil || !changed {
		t.Fatalf("commit changed=%v err=%v", changed, err)
	}
	if _, err := store.Undo(project.ID, doc.Revision); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("restored bytes=%q", got)
	}
}

func TestCheckpointLabelsRevision(t *testing.T) {
	store, _ := projects.NewStore(t.TempDir())
	project, _ := store.Create("Checkpoint")
	if err := store.CreateCheckpoint(project.ID, "Client review", 0); err != nil {
		t.Fatal(err)
	}
	history, err := store.History(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Revisions) != 1 || len(history.Revisions[0].Checkpoints) != 1 {
		t.Fatalf("history=%+v", history)
	}
}
