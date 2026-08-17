package ffmpeg_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	. "parallax/internal/ffmpeg"
)

func TestParseSceneTimesReadsPts(t *testing.T) {
	times, err := ParseSceneTimes(`{"frames":[{"pts_time":"1.00"},{"pkt_pts_time":"2.5"},{"pts_time":"1.000"},{"pts_time":"N/A"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(times) != 2 || times[0] != 1 || times[1] != 2.5 {
		t.Fatalf("times=%v", times)
	}
}

func TestDetectScenesFindsHardCut(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "cut.mp4")
	cmd := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-i", "color=c=red:s=64x64:d=1",
		"-f", "lavfi", "-i", "color=c=blue:s=64x64:d=1",
		"-filter_complex", "[0:v][1:v]concat=n=2:v=1:a=0",
		"-pix_fmt", "yuv420p", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
	cuts, err := DetectScenes(context.Background(), Bins{FFmpeg: "ffmpeg", FFprobe: "ffprobe"}, dir, "cut.mp4", 0.3)
	if err != nil {
		t.Fatal(err)
	}
	if len(cuts) == 0 {
		t.Fatal("expected at least one cut")
	}
	found := false
	for _, cut := range cuts {
		if cut > 0.7 && cut < 1.3 {
			found = true
		}
	}
	if !found {
		t.Fatalf("cuts=%v", cuts)
	}
}

func TestExtractFrameWritesJPEG(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "still.mp4")
	cmd := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "color=c=yellow:s=48x48:d=1", "-pix_fmt", "yuv420p", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
	if err := ExtractFrame(context.Background(), Bins{FFmpeg: "ffmpeg", FFprobe: "ffprobe"}, dir, "still.mp4", "frame.jpg", 0.2); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "frame.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 3 || data[0] != 0xff || data[1] != 0xd8 {
		t.Fatalf("not a jpeg: %d bytes", len(data))
	}
}
