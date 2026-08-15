package ffmpeg

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTokenizeRejectsShell(t *testing.T) {
	bads := []string{
		"ffmpeg -i a.mp4; rm -rf /",
		"ffmpeg -i a.mp4 | cat",
		"ffmpeg -i a.mp4 && echo pwned",
		"ffmpeg -i $(whoami).mp4 out.mp4",
		"ffmpeg -i `id`.mp4 out.mp4",
		"ffmpeg -i a.mp4 > out.mp4",
	}
	for _, c := range bads {
		if _, err := Tokenize(c); err == nil {
			t.Errorf("expected reject: %s", c)
		}
	}
}

func TestTokenizeQuotes(t *testing.T) {
	got, err := Tokenize(`ffmpeg -i "my file.mp4" -an 'out file.mp4'`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ffmpeg", "-i", "my file.mp4", "-an", "out file.mp4"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %#v", got)
	}
}

func TestValidateSandbox(t *testing.T) {
	ws := t.TempDir()
	opts := ValidateOpts{Workspace: ws}

	if _, err := Validate([]string{"ffmpeg", "-i", "../secret.mp4", "out.mp4"}, opts); err == nil {
		t.Fatal("escaped input accepted")
	}
	if _, err := Validate([]string{"-i", "in.mp4", "/etc/passwd"}, opts); err == nil {
		t.Fatal("absolute output accepted")
	}
	if _, err := Validate([]string{"bash", "-c", "id"}, opts); err == nil {
		t.Fatal("non-ffmpeg binary accepted")
	}
	if _, err := Validate([]string{"ffmpeg", "-i", "https://evil.example/a.mp4", "out.mp4"}, opts); err == nil {
		t.Fatal("network input accepted")
	}

	cmd, err := Validate([]string{"ffmpeg", "-y", "-i", "in.mp4", "-c", "copy", "out.mp4"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Kind != KindFFmpeg {
		t.Fatalf("kind=%s", cmd.Kind)
	}
	if len(cmd.Args) == 0 || cmd.Args[0] == "ffmpeg" {
		t.Fatalf("binary should be stripped: %#v", cmd.Args)
	}
}

func TestValidateLavfi(t *testing.T) {
	ws := t.TempDir()
	_, err := Validate([]string{
		"ffmpeg", "-f", "lavfi", "-i", "color=c=black:s=16x16:d=0.1",
		"-frames:v", "1", "frame.png",
	}, ValidateOpts{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
}

func TestValidateFilterGraphPath(t *testing.T) {
	ws := t.TempDir()
	_, err := Validate([]string{
		"ffmpeg", "-i", "in.mp4", "-vf", "subtitles=/etc/passwd", "out.mp4",
	}, ValidateOpts{Workspace: ws})
	if err == nil {
		t.Fatal("filter path escape accepted")
	}
	_, err = Validate([]string{
		"ffmpeg", "-i", "in.mp4", "-vf", "subtitles=talk.srt", "out.mp4",
	}, ValidateOpts{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunLavfiRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	ws := t.TempDir()
	cmd, err := Validate([]string{
		"ffmpeg", "-y", "-f", "lavfi", "-i", "color=c=black:s=32x32:d=0.2",
		"-pix_fmt", "yuv420p", "clip.mp4",
	}, ValidateOpts{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), Bins{}, cmd, ws, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "clip.mp4")); err != nil {
		t.Fatal(err)
	}

	probe, err := Validate([]string{
		"ffprobe", "-v", "error", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", "clip.mp4",
	}, ValidateOpts{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Bins{}, probe, ws, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) == "" {
		t.Fatalf("empty probe stdout: %s", res.Stderr)
	}
}
