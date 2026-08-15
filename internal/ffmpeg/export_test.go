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

func TestBuildExportArgsRejectsBadFormat(t *testing.T) {
	if _, err := BuildExportArgs(ExportSpec{Source: "a.mp4", Format: "exe"}, "out.exe"); err == nil {
		t.Fatal("accepted exe")
	}
}
