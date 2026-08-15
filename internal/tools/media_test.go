package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestListAndInspect(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "talk.mp4"), []byte("not-really-video"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("ignore"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry()
	RegisterMedia(reg, MediaEnv{Workspace: ws})

	listed := reg.Execute(context.Background(), "list_workspace", `{}`)
	if !listed.OK {
		t.Fatal(listed.Error)
	}
	out := listed.Output.(map[string]any)
	if out["count"].(int) != 1 {
		t.Fatalf("count=%v files=%v", out["count"], out["files"])
	}

	ins := reg.Execute(context.Background(), "inspect_file", `{"path":"talk.mp4"}`)
	if !ins.OK {
		t.Fatal(ins.Error)
	}
	escaped := reg.Execute(context.Background(), "inspect_file", `{"path":"../etc/passwd"}`)
	if escaped.OK {
		t.Fatal("path escape succeeded")
	}
}

func TestRunFFmpegLavfiViaTool(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ws := t.TempDir()
	reg := NewRegistry()
	RegisterMedia(reg, MediaEnv{Workspace: ws})

	raw, _ := json.Marshal(map[string]any{
		"rationale": "generate a tiny test clip",
		"args": []string{
			"-y", "-f", "lavfi", "-i", "color=c=black:s=16x16:d=0.2",
			"-pix_fmt", "yuv420p", "clip.mp4",
		},
		"timeout_seconds": 20,
	})
	res := reg.Execute(context.Background(), "run_ffmpeg", string(raw))
	if !res.OK {
		t.Fatalf("%s (%v)", res.Error, res.Output)
	}
	if _, err := os.Stat(filepath.Join(ws, "clip.mp4")); err != nil {
		t.Fatal(err)
	}

	muted := reg.Execute(context.Background(), "run_ffmpeg", mustJSON(map[string]any{
		"rationale": "strip audio as an in-place edit",
		"args": []string{
			"-y", "-i", "clip.mp4", "-c:v", "copy", "-an", "clip_muted.mp4",
		},
		"timeout_seconds": 20,
	}))
	if !muted.OK {
		t.Fatalf("in-place mute: %s (%v)", muted.Error, muted.Output)
	}
	if _, err := os.Stat(filepath.Join(ws, "clip.mp4")); err != nil {
		t.Fatal("source clip was removed instead of being updated")
	}
	if _, err := os.Stat(filepath.Join(ws, "clip_muted.mp4")); !os.IsNotExist(err) {
		t.Fatal("in-place edit left a sibling copy")
	}
	out := muted.Output.(map[string]any)
	if out["applied_to"] != "clip.mp4" {
		t.Fatalf("applied_to=%v", out["applied_to"])
	}

	keep := reg.Execute(context.Background(), "run_ffmpeg", mustJSON(map[string]any{
		"rationale": "keep a separate export",
		"apply_to":  "none",
		"args": []string{
			"-y", "-i", "clip.mp4", "-c:v", "copy", "-an", "highlight.mp4",
		},
		"timeout_seconds": 20,
	}))
	if !keep.OK {
		t.Fatalf("keep output: %s (%v)", keep.Error, keep.Output)
	}
	if _, err := os.Stat(filepath.Join(ws, "highlight.mp4")); err != nil {
		t.Fatal(err)
	}

	// Reject a shell-looking command string.
	bad := reg.Execute(context.Background(), "run_ffmpeg", `{
		"rationale":"should fail",
		"command":"ffmpeg -i clip.mp4; rm -rf /"
	}`)
	if bad.OK {
		t.Fatal("shell command accepted")
	}
}
