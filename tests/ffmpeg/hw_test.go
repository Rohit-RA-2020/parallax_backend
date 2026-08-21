package ffmpeg_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "parallax/internal/ffmpeg"
)

func TestRewriteSoftwareEncodeNVENC(t *testing.T) {
	accel := Accel{Backend: "cuda", H264: "h264_nvenc", HEVC: "hevc_nvenc"}
	got, ok := RewriteSoftwareEncode([]string{
		"-y", "-i", "in.mp4", "-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-pix_fmt", "yuv420p", "out.mp4",
	}, accel)
	if !ok {
		t.Fatal("expected rewrite")
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "h264_nvenc") {
		t.Fatalf("missing nvenc: %s", joined)
	}
	if strings.Contains(joined, "libx264") || strings.Contains(joined, "-crf") || strings.Contains(joined, "veryfast") {
		t.Fatalf("software options left in place: %s", joined)
	}
	if !strings.Contains(joined, "-preset p2") || !strings.Contains(joined, "-cq 20") || !strings.Contains(joined, "-rc vbr") {
		t.Fatalf("nvenc quality mapping: %s", joined)
	}
	if got[len(got)-1] != "out.mp4" {
		t.Fatalf("output moved: %v", got)
	}
}

func TestPreviewEncodePlanReportsSelectedDevice(t *testing.T) {
	got := PreviewEncodePlan(Bins{Accel: Accel{
		Backend: "cuda",
		Device:  "0",
		Label:   "NVIDIA L4",
		H264:    "h264_nvenc",
	}})
	if !got.Hardware || got.Encoder != "h264_nvenc" || got.Device != "NVIDIA L4 (0)" {
		t.Fatalf("preview plan=%+v", got)
	}
}

func TestRewriteSoftwareEncodeLeavesCopy(t *testing.T) {
	accel := Accel{Backend: "cuda", H264: "h264_nvenc"}
	args := []string{"-y", "-i", "in.mp4", "-c:v", "copy", "-an", "out.mp4"}
	got, ok := RewriteSoftwareEncode(args, accel)
	if ok {
		t.Fatalf("copy should not rewrite: %v", got)
	}
}

func TestRewriteSoftwareEncodeSkipsExistingHW(t *testing.T) {
	accel := Accel{Backend: "cuda", H264: "h264_nvenc"}
	args := []string{"-y", "-i", "in.mp4", "-c:v", "h264_nvenc", "out.mp4"}
	if _, ok := RewriteSoftwareEncode(args, accel); ok {
		t.Fatal("already-hw argv rewritten")
	}
}

func TestRewriteSoftwareEncodeSkipsVAAPIFilterComplex(t *testing.T) {
	accel := Accel{Backend: "vaapi", Device: "/dev/dri/renderD128", H264: "h264_vaapi"}
	args := []string{"-y", "-filter_complex", "[0:v]scale=1280:720[v]", "-map", "[v]", "-c:v", "libx264", "out.mp4"}
	if _, ok := RewriteSoftwareEncode(args, accel); ok {
		t.Fatal("vaapi should not splice into filter_complex")
	}
}

func TestRewriteSoftwareEncodeVAAPISimple(t *testing.T) {
	accel := Accel{Backend: "vaapi", Device: "/dev/dri/renderD128", H264: "h264_vaapi"}
	got, ok := RewriteSoftwareEncode([]string{
		"-y", "-hide_banner", "-i", "in.mp4", "-vf", "scale=1280:720", "-c:v", "libx264", "-crf", "18", "out.mp4",
	}, accel)
	if !ok {
		t.Fatal("expected vaapi rewrite")
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "h264_vaapi") || !strings.Contains(joined, "hwupload") || !strings.Contains(joined, "-init_hw_device") {
		t.Fatalf("vaapi rewrite incomplete: %s", joined)
	}
	if !strings.Contains(joined, "-qp 18") {
		t.Fatalf("missing qp: %s", joined)
	}
	if strings.Index(joined, "-init_hw_device") > strings.Index(joined, "-i in.mp4") {
		t.Fatalf("hw device must precede inputs: %s", joined)
	}
}

func TestRewriteSoftwareEncodeHEVCAndDisabledFamily(t *testing.T) {
	accel := Accel{Backend: "cuda", H264: "h264_nvenc"}
	got, ok := RewriteSoftwareEncode([]string{"-y", "-i", "in.mp4", "-c:v", "libx265", "out.mp4"}, accel)
	if ok {
		t.Fatalf("hevc should stay on cpu without hevc encoder: %v", got)
	}
	accel.HEVC = "hevc_nvenc"
	got, ok = RewriteSoftwareEncode([]string{"-y", "-i", "in.mp4", "-c:v", "libx265", "out.mp4"}, accel)
	if !ok || !containsArg(got, "hevc_nvenc") {
		t.Fatalf("hevc rewrite: ok=%v %v", ok, got)
	}
}

func TestRewriteSoftwareEncodeOff(t *testing.T) {
	args := []string{"-y", "-i", "in.mp4", "-c:v", "libx264", "out.mp4"}
	if _, ok := RewriteSoftwareEncode(args, Accel{}); ok {
		t.Fatal("zero accel rewrote")
	}
}

func TestDetectAccelOff(t *testing.T) {
	if got := DetectAccel(context.Background(), Bins{FFmpeg: "ffmpeg"}, DetectOpts{Prefer: "off"}); got.Enabled() {
		t.Fatalf("off should disable: %+v", got)
	}
}

func TestDetectAccelAndEncode(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	accel := DetectAccel(ctx, Bins{FFmpeg: "ffmpeg"}, DetectOpts{Prefer: "auto"})
	if !accel.Enabled() {
		t.Skip("no working ffmpeg GPU encoder on this host")
	}
	if accel.H264 == "" {
		t.Fatalf("enabled accel missing h264: %+v", accel)
	}

	ws := t.TempDir()
	args := []string{"-y", "-f", "lavfi", "-i", "color=c=black:s=64x64:d=0.2", "-c:v", "libx264", "-pix_fmt", "yuv420p", "gpu.mp4"}
	cmd, err := Validate(args, ValidateOpts{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(ctx, Bins{FFmpeg: "ffmpeg", Accel: accel}, cmd, ws, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(res.Args, " ")
	if !strings.Contains(joined, accel.H264) {
		t.Fatalf("run did not use %s: %s", accel.H264, joined)
	}
	if _, err := os.Stat(filepath.Join(ws, "gpu.mp4")); err != nil {
		t.Fatal(err)
	}
}

func TestRunFallsBackToCPUWhenGPUEncoderMissing(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ws := t.TempDir()
	args := []string{"-y", "-f", "lavfi", "-i", "color=c=black:s=32x32:d=0.2", "-c:v", "libx264", "-pix_fmt", "yuv420p", "cpu.mp4"}
	cmd, err := Validate(args, ValidateOpts{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	accel := Accel{Backend: "cuda", H264: "h264_nvenc_missing"}
	res, err := Run(context.Background(), Bins{FFmpeg: "ffmpeg", Accel: accel}, cmd, ws, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stderr, "retried on cpu") {
		t.Fatalf("expected cpu fallback note: %s", res.Stderr)
	}
	if _, err := os.Stat(filepath.Join(ws, "cpu.mp4")); err != nil {
		t.Fatal(err)
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
