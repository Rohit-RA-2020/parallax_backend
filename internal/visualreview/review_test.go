package visualreview

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	"parallax/internal/ffmpeg"
	"parallax/internal/projects"
)

func TestDeterministicFindingsDetectsBlackFrameAndBrightnessJump(t *testing.T) {
	group := []Frame{
		{ID: "before", Time: 1, AvgLuma: .5},
		{ID: "at", Time: 1.04, AvgLuma: .01},
		{ID: "after", Time: 1.08, AvgLuma: .9},
	}
	findings := deterministicFindings([][]Frame{group})
	if len(findings) != 2 {
		t.Fatalf("expected black-frame and brightness findings, got %#v", findings)
	}
	if findings[0].Type != "black_frame" && findings[1].Type != "black_frame" {
		t.Fatalf("expected black_frame finding, got %#v", findings)
	}
}

func TestChangedFocusTimesIncludesAddedAndModifiedClips(t *testing.T) {
	previous := projects.Timeline{FPS: 24, Canvas: projects.TimelineCanvas{Width: 1920, Height: 1080}, Clips: []projects.TimelineClip{{ID: "a", Track: "V1", Kind: "video", StartFrame: 0, DurationFrames: 48}}}
	current := projects.Timeline{FPS: 24, Canvas: previous.Canvas, Clips: []projects.TimelineClip{
		{ID: "a", Track: "V1", Kind: "video", StartFrame: 0, DurationFrames: 36},
		{ID: "b", Track: "V1", Kind: "video", StartFrame: 36, DurationFrames: 48},
	}}
	focus := changedFocusTimes(previous, current)
	if len(focus) != 3 {
		t.Fatalf("expected three unique changed boundaries, got %v", focus)
	}
	if focus[0] != 0 || focus[1] != 1.5 || focus[2] != 3.5 {
		t.Fatalf("unexpected focus times: %v", focus)
	}
}

func TestAverageLuma(t *testing.T) {
	dark := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			dark.Set(x, y, color.RGBA{R: 10, G: 10, B: 10, A: 255})
		}
	}
	if got := averageLuma(dark); got < .03 || got > .05 {
		t.Fatalf("unexpected luma: %f", got)
	}
}

func TestReviewRendersAndPersistsImageEvidence(t *testing.T) {
	store, err := projects.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.Create("Visual review")
	if err != nil {
		t.Fatal(err)
	}
	still := image.NewRGBA(image.Rect(0, 0, 64, 36))
	for y := 0; y < 36; y++ {
		for x := 0; x < 64; x++ {
			still.Set(x, y, color.RGBA{R: 220, G: 80, B: 40, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, still); err != nil {
		t.Fatal(err)
	}
	media, err := store.SaveUpload(project.ID, "still.png", bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	doc := projects.Timeline{Schema: 2, FPS: 24, Canvas: projects.TimelineCanvas{Width: 1920, Height: 1080}, Clips: []projects.TimelineClip{{
		ID: "still", Name: "still", Track: "V1", Kind: "video", StartFrame: 0, DurationFrames: 120, SourceDurationFrames: 120, MediaPath: media.Path, MediaType: "image",
	}}}
	saved, err := store.SaveTimeline(project.ID, doc)
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Service{Store: store, Bins: ffmpeg.Bins{FFmpeg: "ffmpeg"}}).Review(t.Context(), Request{ProjectID: project.ID, Revision: saved.Revision, Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Frames) == 0 {
		t.Fatalf("expected rendered evidence, result=%+v", result)
	}
	if result.Status != "degraded" {
		t.Fatalf("expected degraded status without a vision provider, got %q", result.Status)
	}
	loaded, err := (&Service{Store: store}).Load(project.ID, saved.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != saved.Revision || len(loaded.Frames) != len(result.Frames) {
		t.Fatalf("loaded=%+v result=%+v", loaded, result)
	}
}
