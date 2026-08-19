package ffmpeg_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	. "parallax/internal/ffmpeg"
)

func TestParseMediaProbeVideo(t *testing.T) {
	got, err := ParseMediaProbe(`{
		"streams":[{"width":1080,"height":1920,"codec_type":"video"}],
		"format":{"duration":"30.533000"}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if got.Width != 1080 || got.Height != 1920 {
		t.Fatalf("frame=%dx%d", got.Width, got.Height)
	}
	if got.Duration < 30.5 || got.Duration > 30.6 {
		t.Fatalf("duration=%v", got.Duration)
	}
	if !got.HasVideo || got.HasAudio {
		t.Fatalf("streams video=%v audio=%v", got.HasVideo, got.HasAudio)
	}
}

func TestParseMediaProbeReadsCodec(t *testing.T) {
	got, err := ParseMediaProbe(`{
		"streams":[
			{"codec_type":"video","codec_name":"hevc","pix_fmt":"yuv420p10le","width":1920,"height":1080},
			{"codec_type":"audio","codec_name":"eac3"}
		],
		"format":{"duration":"12.5"}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if got.VideoCodec != "hevc" || got.PixelFormat != "yuv420p10le" || got.AudioCodec != "eac3" {
		t.Fatalf("probe=%+v", got)
	}
	if BrowserPlayable("media/film.mkv", got) {
		t.Fatal("mkv/hevc 10-bit should need a preview proxy")
	}
	if PreviewReason("media/film.mkv", got) == "" {
		t.Fatal("expected a reason")
	}
	h264 := MediaProbe{HasVideo: true, VideoCodec: "h264", PixelFormat: "yuv420p"}
	if !BrowserPlayable("media/talk.mp4", h264) {
		t.Fatal("h264 mp4 should play natively")
	}
	if BrowserPlayable("media/talk.mkv", h264) {
		t.Fatal("mkv should not play natively even with h264")
	}
}

func TestWritePreviewMakesH264Mp4(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	cmd := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-i", "color=c=red:s=320x180:d=0.4",
		"-f", "lavfi", "-i", "sine=f=440:d=0.4",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-shortest", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("src: %v\n%s", err, out)
	}
	bins := Bins{FFmpeg: "ffmpeg", FFprobe: "ffprobe"}
	if err := WritePreview(context.Background(), bins, dir, "src.mkv", "prev.mp4", 0.4, nil); err != nil {
		t.Fatal(err)
	}
	info, err := ProbeMedia(context.Background(), bins, dir, "prev.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if info.VideoCodec != "h264" || !BrowserPlayable("prev.mp4", info) {
		t.Fatalf("preview=%+v", info)
	}
}

func TestParseMediaProbeSkipsAudioStream(t *testing.T) {
	got, err := ParseMediaProbe(`{
		"streams":[
			{"codec_type":"audio"},
			{"width":1920,"height":1080,"codec_type":"video"}
		],
		"format":{"duration":"8"}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if got.Width != 1920 || got.Height != 1080 {
		t.Fatalf("frame=%dx%d", got.Width, got.Height)
	}
	if !got.HasVideo || !got.HasAudio {
		t.Fatalf("streams video=%v audio=%v", got.HasVideo, got.HasAudio)
	}
}
