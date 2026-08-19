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
	idx.MarkProgress(project.ID, "media/talk.mp4", 72, 900)
	if idx.Statuses(project.ID)["media/talk.mp4"].Progress != "1:12 / 15:00" {
		t.Fatalf("progress=%+v", idx.Statuses(project.ID)["media/talk.mp4"])
	}
	idx.Clear(project.ID, "media/talk.mp4")
	if _, ok := idx.Statuses(project.ID)["media/talk.mp4"]; ok {
		t.Fatal("status should be cleared")
	}
}

func TestNoteUploadAndTimingsSurviveMark(t *testing.T) {
	store, err := projects.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.Create("Talk")
	if err != nil {
		t.Fatal(err)
	}
	idx := &Indexer{Projects: store}
	idx.NoteUpload(project.ID, "media/talk.mp4", 1400)
	idx.Mark(project.ID, "media/talk.mp4", StateQueued, "")
	idx.AddTiming(project.ID, "media/talk.mp4", TimingExtract, 200)
	idx.AddTiming(project.ID, "media/talk.mp4", TimingTranscribe, 5300)
	idx.AddTiming(project.ID, "media/talk.mp4", TimingIndex, 400)
	idx.AddTiming(project.ID, "media/talk.mp4", TimingIndex, 150)
	idx.SetTimingMeta(project.ID, "media/talk.mp4", "large-v3-turbo", "cuda")
	idx.MarkProgress(project.ID, "media/talk.mp4", 12, 90)
	got := idx.Statuses(project.ID)["media/talk.mp4"]
	if got.Timings.UploadMs != 1400 || got.Timings.ExtractMs != 200 || got.Timings.TranscribeMs != 5300 || got.Timings.IndexMs != 550 {
		t.Fatalf("progress wiped timings: %+v", got.Timings)
	}
	if got.Timings.Model != "large-v3-turbo" || got.Timings.Device != "cuda" {
		t.Fatalf("meta=%+v", got.Timings)
	}
	idx.Mark(project.ID, "media/talk.mp4", StateReady, "")
	got = idx.Statuses(project.ID)["media/talk.mp4"]
	if got.Timings.TotalMs < 1400+200+5300+550 {
		t.Fatalf("total=%+v", got.Timings)
	}
	idx.Mark(project.ID, "media/talk.mp4", StateQueued, "")
	idx.Mark(project.ID, "media/talk.mp4", StateReady, "")
	got = idx.Statuses(project.ID)["media/talk.mp4"]
	if got.Timings.TranscribeMs != 5300 || got.Timings.TotalMs < 5300 {
		t.Fatalf("requeue should keep last run timings: %+v", got.Timings)
	}
}

func TestRemoveProjectClearsLiveStatus(t *testing.T) {
	store, err := projects.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.Create("Talk")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Create("Keep")
	if err != nil {
		t.Fatal(err)
	}
	idx := &Indexer{Projects: store}
	idx.Mark(project.ID, "media/talk.mp4", StateReady, "")
	idx.Mark(other.ID, "media/keep.mp4", StateReady, "")
	if err := idx.RemoveProject(t.Context(), project.ID); err != nil {
		t.Fatal(err)
	}
	if len(idx.Statuses(project.ID)) != 0 {
		t.Fatalf("removed=%+v", idx.Statuses(project.ID))
	}
	if idx.Statuses(other.ID)["media/keep.mp4"].State != StateReady {
		t.Fatalf("kept=%+v", idx.Statuses(other.ID))
	}
}
