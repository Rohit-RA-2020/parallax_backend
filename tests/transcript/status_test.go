package transcript_test

import (
	"testing"

	"parallax/internal/projects"
	. "parallax/internal/transcript"
)

func TestMarkPersistsAndLiveOverrides(t *testing.T) {
	store, err := projects.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.Create("Talk")
	if err != nil {
		t.Fatal(err)
	}
	idx := &Indexer{Projects: store}
	idx.Mark(project.ID, "media/talk.mp4", StateQueued, "")
	got := idx.Statuses(project.ID)
	if got["media/talk.mp4"].State != StateQueued {
		t.Fatalf("queued=%+v", got)
	}
	idx.Mark(project.ID, "media/talk.mp4", StateReady, "")
	got = idx.Statuses(project.ID)
	if got["media/talk.mp4"].State != StateReady || got["media/talk.mp4"].Error != "" {
		t.Fatalf("ready=%+v", got)
	}
	idx.Clear(project.ID, "media/talk.mp4")
	if _, ok := idx.Statuses(project.ID)["media/talk.mp4"]; ok {
		t.Fatal("status should be cleared")
	}
}
