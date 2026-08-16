package ffmpeg

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ExtractMono16k writes a 16 kHz mono PCM wav for ASR. outRel is workspace-relative.
func ExtractMono16k(ctx context.Context, bins Bins, workspace, inRel, outRel string) error {
	if err := os.MkdirAll(filepath.Join(workspace, filepath.Dir(filepath.FromSlash(outRel))), 0o755); err != nil {
		return err
	}
	cmd, err := Validate([]string{
		"ffmpeg", "-y",
		"-i", inRel,
		"-vn", "-ac", "1", "-ar", "16000",
		"-c:a", "pcm_s16le",
		outRel,
	}, ValidateOpts{Workspace: workspace})
	if err != nil {
		return fmt.Errorf("extract audio: %w", err)
	}
	if _, err := Run(ctx, bins, cmd, workspace, 10*time.Minute); err != nil {
		return fmt.Errorf("extract audio: %w", err)
	}
	return nil
}
