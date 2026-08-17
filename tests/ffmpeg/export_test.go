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

func TestBuildExportArgsMP4(t *testing.T) {
	args, err := BuildExportArgs(ExportSpec{
		Source:     "media/talk.mp4",
		Format:     "mp4",
		Quality:    "standard",
		Resolution: "1920x1080",
		FPS:        24,
		Audio:      true,
	}, "exports/talk.mp4")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-i media/talk.mp4") {
		t.Fatalf("missing input: %s", joined)
	}
	if !strings.Contains(joined, "libx264") || !strings.Contains(joined, "scale=1920:1080") {
		t.Fatalf("encode args: %s", joined)
	}
	if args[len(args)-1] != "exports/talk.mp4" {
		t.Fatalf("dest=%v", args)
	}
	if _, err := Validate(args, ValidateOpts{Workspace: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
}

func TestBuildExportArgsOriginalCopy(t *testing.T) {
	args, err := BuildExportArgs(ExportSpec{
		Source:  "media/talk.mp4",
		Format:  "mp4",
		Quality: "original",
		Audio:   false,
	}, "exports/talk.mp4")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-c:v copy") || !strings.Contains(joined, "-an") {
		t.Fatalf("copy args: %s", joined)
	}
}

func TestBuildSequenceArgsCompositesProgram(t *testing.T) {
	args, err := BuildSequenceArgs(ExportSpec{
		Source:     SequenceSource,
		Format:     "mp4",
		Quality:    "draft",
		Resolution: "1280x720",
		FPS:        24,
		Audio:      true,
	}, []SequenceClip{
		{Track: "V1", Kind: "video", Path: "media/a.mp4", Start: 0, Duration: 2, SourceIn: 1},
		{Track: "V1", Kind: "video", Path: "media/b.mp4", Start: 3, Duration: 2},
		{Track: "V2", Kind: "title", Name: "SALT ROAD", Start: 0.5, Duration: 1},
		{Track: "A1", Kind: "audio", Path: "media/a.mp4", Start: 3, Duration: 2, SourceIn: 1},
	}, "exports/seq.mp4")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "color=c=black") {
		t.Fatalf("missing program canvas: %s", joined)
	}
	if !strings.Contains(joined, "overlay=") || !strings.Contains(joined, "drawtext=") {
		t.Fatalf("missing V1/V2 composite: %s", joined)
	}
	if !strings.Contains(joined, "amix=") || !strings.Contains(joined, "adelay=") {
		t.Fatalf("missing A mix: %s", joined)
	}
	if !strings.Contains(joined, "-i media/a.mp4") || !strings.Contains(joined, "-i media/b.mp4") {
		t.Fatalf("missing clip inputs: %s", joined)
	}
	if _, err := Validate(args, ValidateOpts{Workspace: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
}

func TestBuildSequenceArgsBurnsCaptionTrack(t *testing.T) {
	args, err := BuildSequenceArgs(ExportSpec{
		Source:     SequenceSource,
		Format:     "mp4",
		Quality:    "draft",
		Resolution: "1280x720",
		FPS:        24,
		Audio:      false,
		Captions:   "burn",
	}, []SequenceClip{
		{Track: "V1", Kind: "video", Path: "media/a.mp4", Start: 0, Duration: 2},
		{Track: "C1", Kind: "caption", SubtitlePath: ".scratch/export-cap-0.srt", CaptionLang: "hi", Fill: "#ffcc00", FontSize: 28, Start: 0, Duration: 2},
	}, "exports/seq.mp4")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "subtitles=") || !strings.Contains(joined, "export-cap-0.srt") {
		t.Fatalf("missing caption burn: %s", joined)
	}
	if !strings.Contains(joined, "Fontsize=28") {
		t.Fatalf("burn should use the clip caption size: %s", joined)
	}
	if !strings.Contains(joined, "PrimaryColour=&H0000CCFF&") {
		t.Fatalf("burn should use the clip fill color: %s", joined)
	}
	if strings.Contains(joined, "-c:s") {
		t.Fatalf("burn should not mux a subtitle stream: %s", joined)
	}
	if _, err := Validate(args, ValidateOpts{Workspace: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
}

func TestBuildSequenceArgsMuxesSelectableTrack(t *testing.T) {
	args, err := BuildSequenceArgs(ExportSpec{
		Source:     SequenceSource,
		Format:     "mp4",
		Quality:    "draft",
		Resolution: "1280x720",
		FPS:        24,
		Audio:      false,
		Captions:   "soft",
		Subtitles: []ExportSubtitle{{
			Path:     ".scratch/export-cap-hi.srt",
			Language: "hin",
			Title:    "Hindi",
		}},
	}, []SequenceClip{
		{Track: "V1", Kind: "video", Path: "media/a.mp4", Start: 0, Duration: 2},
		{Track: "C1", Kind: "caption", Start: 0, Duration: 2},
	}, "exports/seq.mp4")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "subtitles=") {
		t.Fatalf("soft export should not burn: %s", joined)
	}
	if !strings.Contains(joined, "-i .scratch/export-cap-hi.srt") {
		t.Fatalf("missing subtitle input: %s", joined)
	}
	if !strings.Contains(joined, "-c:s mov_text") || !strings.Contains(joined, "language=hin") || !strings.Contains(joined, "title=Hindi") {
		t.Fatalf("missing selectable track: %s", joined)
	}
	if !strings.Contains(joined, "-disposition:s:0 default") {
		t.Fatalf("track should default on: %s", joined)
	}
	if _, err := Validate(args, ValidateOpts{Workspace: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
}

func TestBuildExportArgsMuxesSubtitles(t *testing.T) {
	args, err := BuildExportArgs(ExportSpec{
		Source:    "media/talk.mp4",
		Format:    "mp4",
		Quality:   "original",
		Audio:     true,
		Captions:  "soft",
		Subtitles: []ExportSubtitle{{Path: ".scratch/hi.srt", Language: "hin", Title: "Hindi"}},
	}, "exports/talk.mp4")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-i .scratch/hi.srt") || !strings.Contains(joined, "-c:s mov_text") {
		t.Fatalf("source export missing track: %s", joined)
	}
	if !strings.Contains(joined, "-c:v copy") {
		t.Fatalf("original+soft should still copy picture: %s", joined)
	}
}

func TestExportMuxesSelectableMovText(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	ws := t.TempDir()
	seed, err := Validate([]string{
		"ffmpeg", "-y", "-f", "lavfi", "-i", "color=c=black:s=32x32:d=1",
		"-pix_fmt", "yuv420p", "talk.mp4",
	}, ValidateOpts{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), Bins{}, seed, ws, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "hi.srt"), []byte("1\n00:00:00,000 --> 00:00:00,800\nनमस्ते\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args, err := BuildExportArgs(ExportSpec{
		Source:    "talk.mp4",
		Format:    "mp4",
		Quality:   "original",
		Audio:     false,
		Captions:  "soft",
		Subtitles: []ExportSubtitle{{Path: "hi.srt", Language: "hin", Title: "Hindi"}},
	}, "out.mp4")
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := Validate(args, ValidateOpts{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), Bins{}, cmd, ws, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	probe, err := Validate([]string{
		"ffprobe", "-v", "error", "-select_streams", "s",
		"-show_entries", "stream=codec_name:stream_tags=language,title,handler_name",
		"-of", "csv=p=0", "out.mp4",
	}, ValidateOpts{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Bins{}, probe, ws, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	out := strings.ToLower(res.Stdout + res.Stderr)
	if !strings.Contains(out, "mov_text") {
		t.Fatalf("exported file has no selectable subtitle track: %q", res.Stdout+" "+res.Stderr)
	}
	if !strings.Contains(out, "hin") && !strings.Contains(out, "hi") {
		t.Fatalf("subtitle language missing: %q", res.Stdout)
	}
}

func TestBuildSequenceArgsRejectsEmpty(t *testing.T) {
	if _, err := BuildSequenceArgs(ExportSpec{Source: SequenceSource, Format: "mp4"}, nil, "exports/x.mp4"); err == nil {
		t.Fatal("empty sequence accepted")
	}
}

func TestBuildExportArgsRejectsBadFormat(t *testing.T) {
	if _, err := BuildExportArgs(ExportSpec{Source: "a.mp4", Format: "exe"}, "out.exe"); err == nil {
		t.Fatal("accepted exe")
	}
}
