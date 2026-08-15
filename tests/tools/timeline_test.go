package tools_test

import (
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
