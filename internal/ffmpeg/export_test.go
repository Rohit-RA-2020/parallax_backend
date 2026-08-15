package ffmpeg

import (
	"strings"
	"testing"
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
