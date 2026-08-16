package tools_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"parallax/internal/projects"
	"parallax/internal/tools"
)

func TestTimelineToolStagesThenCommitsOneRevision(t *testing.T) {
	store, err := projects.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.Create("Director")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.BeginTimelineTransaction(project.ID, projects.CommitMeta{Actor: "agent", Summary: "Add fading title"})
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	tools.RegisterTimeline(registry, tools.TimelineEnv{Transaction: tx})
	raw := `{"operations":[{"type":"add_item","item":{"name":"Welcome","track":"V2","kind":"title","start_frame":0,"duration_frames":120,"source_in_frame":0,"title":{"text":"Welcome","font_size":64,"fill":"#ffffff"},"transform":{"x":960,"y":96,"anchor_x":0.5,"scale_x":1,"scale_y":1,"opacity":1},"keyframes":[{"property":"transform.opacity","frame":0,"value":0},{"property":"transform.opacity","frame":120,"value":1,"easing":"ease_in_out"}]}}]}`
	result := registry.Execute(t.Context(), "edit_timeline", raw)
	if !result.OK {
		t.Fatalf("tool error=%s", result.Error)
	}
	before, _ := store.GetTimeline(project.ID)
	if len(before.Clips) != 0 {
		t.Fatal("staged edit leaked before commit")
	}
	after, changed, err := tx.Commit()
	if err != nil || !changed {
		t.Fatalf("commit changed=%v err=%v", changed, err)
	}
	if after.Revision != 1 || len(after.Clips) != 1 || after.Clips[0].Title.Text != "Welcome" {
		t.Fatalf("timeline=%+v", after)
	}
}

func TestPlaceMediaPutsLinkedAudioAndFocusesPreview(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	store, err := projects.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.Create("Place")
	if err != nil {
		t.Fatal(err)
	}
	mediaDir := filepath.Join(project.Dir, "media")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(mediaDir, "talk.mp4")
	cmd := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-i", "color=c=black:s=32x32:d=1",
		"-f", "lavfi", "-i", "sine=f=440:d=1",
		"-shortest", "-pix_fmt", "yuv420p", out)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, data)
	}
	tx, err := store.BeginTimelineTransaction(project.ID, projects.CommitMeta{Actor: "agent", Summary: "Place media"})
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	tools.RegisterTimeline(registry, tools.TimelineEnv{
		Transaction: tx,
		Store:       store,
		ProjectID:   project.ID,
		Workspace:   project.Dir,
	})
	result := registry.Execute(t.Context(), "place_media", `{"path":"media/talk.mp4"}`)
	if !result.OK {
		t.Fatalf("place_media: %s", result.Error)
	}
	doc, changed, err := tx.Commit()
	if err != nil || !changed {
		t.Fatalf("commit changed=%v err=%v", changed, err)
	}
	if len(doc.Clips) != 2 {
		t.Fatalf("clips=%d %+v", len(doc.Clips), doc.Clips)
	}
	var video, audio *projects.TimelineClip
	for i := range doc.Clips {
		switch doc.Clips[i].Track {
		case "V1":
			video = &doc.Clips[i]
		case "A1":
			audio = &doc.Clips[i]
		}
	}
	if video == nil || audio == nil || video.LinkID == "" || video.LinkID != audio.LinkID {
		t.Fatalf("video=%+v audio=%+v", video, audio)
	}
	if video.DurationFrames < 20 || audio.MediaPath != "media/talk.mp4" {
		t.Fatalf("duration=%d path=%s", video.DurationFrames, audio.MediaPath)
	}
	if doc.PlayheadFrame != video.StartFrame || doc.SelectedID != video.ID {
		t.Fatalf("preview focus playhead=%d selected=%s", doc.PlayheadFrame, doc.SelectedID)
	}
}

func TestAddItemVideoExpandsLinkedAudio(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	store, err := projects.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.Create("Expand")
	if err != nil {
		t.Fatal(err)
	}
	mediaDir := filepath.Join(project.Dir, "media")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(mediaDir, "talk.mp4")
	cmd := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-i", "color=c=black:s=32x32:d=1",
		"-f", "lavfi", "-i", "sine=f=440:d=1",
		"-shortest", "-pix_fmt", "yuv420p", out)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, data)
	}
	tx, err := store.BeginTimelineTransaction(project.ID, projects.CommitMeta{Actor: "agent", Summary: "Add item"})
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	tools.RegisterTimeline(registry, tools.TimelineEnv{
		Transaction: tx,
		Workspace:   project.Dir,
	})
	payload, _ := json.Marshal(map[string]any{
		"operations": []map[string]any{{
			"type": "add_item",
			"item": map[string]any{
				"name":            "talk",
				"track":           "V1",
				"kind":            "video",
				"start_frame":     0,
				"duration_frames": 1,
				"media_path":      "media/talk.mp4",
			},
		}},
	})
	result := registry.Execute(t.Context(), "edit_timeline", string(payload))
	if !result.OK {
		t.Fatalf("edit_timeline: %s", result.Error)
	}
	doc, _, err := tx.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Clips) != 2 {
		t.Fatalf("clips=%d %+v", len(doc.Clips), doc.Clips)
	}
	if doc.Clips[0].DurationFrames < 20 {
		t.Fatalf("duration not repaired from probe: %d", doc.Clips[0].DurationFrames)
	}
}

func TestDirectorHistoryToolStagesUndo(t *testing.T) {
	store, _ := projects.NewStore(t.TempDir())
	project, _ := store.Create("Undo")
	_, err := store.SaveTimelineCommit(project.ID, projects.Timeline{Schema: 2, FPS: 24, Clips: []projects.TimelineClip{{ID: "a", Name: "A", Track: "V1", Kind: "video", DurationFrames: 24}}}, 0, projects.CommitMeta{Summary: "Add"})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.BeginTimelineTransaction(project.ID, projects.CommitMeta{Actor: "agent", Summary: "Undo"})
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	tools.RegisterTimeline(registry, tools.TimelineEnv{Transaction: tx, Store: store, ProjectID: project.ID})
	result := registry.Execute(t.Context(), "undo_project_change", `{}`)
	if !result.OK {
		t.Fatalf("undo tool: %s", result.Error)
	}
	doc, changed, err := tx.Commit()
	if err != nil || !changed {
		t.Fatalf("commit changed=%v err=%v", changed, err)
	}
	if doc.Revision != 0 || len(doc.Clips) != 0 {
		t.Fatalf("timeline=%+v", doc)
	}
}
