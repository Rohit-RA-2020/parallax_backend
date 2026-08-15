package projects

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTimelinePersistsAcrossReload(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.Create("Cut")
	if err != nil {
		t.Fatal(err)
	}

	empty, err := store.GetTimeline(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Revision != 0 || empty.FPS != 24 || len(empty.Clips) != 0 {
		t.Fatalf("empty=%+v", empty)
	}

	saved, err := store.SaveTimeline(p.ID, Timeline{
		FPS:           24,
		PlayheadFrame: 77,
		SelectedID:    "clip-a",
		PxPerSecond:   32,
		Clips: []TimelineClip{
			{
				ID:                   "clip-b",
				Name:                 "Score",
				Track:                "A1",
				Kind:                 "audio",
				StartFrame:           24,
				DurationFrames:       48,
				SourceInFrame:        12,
				SourceDurationFrames: 240,
				MediaPath:            "media/score.wav",
				MediaType:            "audio",
				Color:                "#3d8f72",
				WaveSeed:             9,
			},
			{
				ID:                   "clip-a",
				Name:                 "Highway",
				Track:                "V1",
				Kind:                 "video",
				StartFrame:           0,
				DurationFrames:       96,
				SourceInFrame:        8,
				SourceDurationFrames: 288,
				MediaPath:            "media/highway.mp4",
				MediaType:            "video",
				Color:                "#8a6a48",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 1 {
		t.Fatalf("revision=%d", saved.Revision)
	}
	if saved.UpdatedAt.IsZero() {
		t.Fatal("expected updated_at")
	}
	if len(saved.Clips) != 2 || saved.Clips[0].ID != "clip-a" || saved.Clips[1].ID != "clip-b" {
		t.Fatalf("clips should be ordered by start: %+v", saved.Clips)
	}

	reloaded, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reloaded.GetTimeline(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 1 || got.PlayheadFrame != 77 || got.SelectedID != "clip-a" || got.PxPerSecond != 32 {
		t.Fatalf("got=%+v", got)
	}
	if got.Clips[0].SourceInFrame != 8 || got.Clips[0].DurationFrames != 96 || got.Clips[0].MediaPath != "media/highway.mp4" {
		t.Fatalf("video clip=%+v", got.Clips[0])
	}
	if got.Clips[1].WaveSeed != 9 || got.Clips[1].SourceInFrame != 12 {
		t.Fatalf("audio clip=%+v", got.Clips[1])
	}

	again, err := reloaded.SaveTimeline(p.ID, got)
	if err != nil {
		t.Fatal(err)
	}
	if again.Revision != 2 {
		t.Fatalf("revision=%d", again.Revision)
	}
	if _, err := os.Stat(filepath.Join(p.Dir, ".parallax", "timeline.json.tmp")); !os.IsNotExist(err) {
		t.Fatalf("tmp file left behind: %v", err)
	}
}

func TestTimelineRejectsInvalid(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.Create("Bad")
	if err != nil {
		t.Fatal(err)
	}

	cases := []Timeline{
		{FPS: 24, Clips: []TimelineClip{{ID: "a", Track: "V9", Kind: "video", DurationFrames: 1}}},
		{FPS: 24, Clips: []TimelineClip{{ID: "a", Track: "V1", Kind: "audio", DurationFrames: 1}}},
		{FPS: 24, Clips: []TimelineClip{{ID: "../x", Track: "V1", Kind: "video", DurationFrames: 1}}},
		{FPS: 24, Clips: []TimelineClip{{ID: "a", Track: "V1", Kind: "video", DurationFrames: 0}}},
		{FPS: 24, Clips: []TimelineClip{{ID: "a", Track: "V1", Kind: "video", StartFrame: -1, DurationFrames: 1}}},
		{FPS: 24, Clips: []TimelineClip{{ID: "a", Track: "V1", Kind: "video", DurationFrames: 10, MediaPath: "../secret.mp4"}}},
		{Schema: 9, FPS: 24},
		{
			FPS: 24,
			Clips: []TimelineClip{
				{ID: "a", Track: "V1", Kind: "video", DurationFrames: 1},
				{ID: "a", Track: "V1", Kind: "video", StartFrame: 10, DurationFrames: 1},
			},
		},
	}
	for i, doc := range cases {
		if _, err := store.SaveTimeline(p.ID, doc); err == nil {
			t.Fatalf("case %d accepted: %+v", i, doc)
		}
	}
}

func TestTimelineClampsSourceOverflow(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.Create("Clamp")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.SaveTimeline(p.ID, Timeline{
		FPS: 24,
		Clips: []TimelineClip{{
			ID:                   "a",
			Track:                "V1",
			Kind:                 "video",
			StartFrame:           0,
			DurationFrames:       100,
			SourceInFrame:        10,
			SourceDurationFrames: 50,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Clips[0].DurationFrames != 40 {
		t.Fatalf("duration=%d", saved.Clips[0].DurationFrames)
	}
}
