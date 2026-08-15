package ffmpeg

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// ProbeDuration returns the container duration in seconds for a workspace file.
func ProbeDuration(ctx context.Context, bins Bins, workspace, rel string) (float64, error) {
	cmd, err := Validate([]string{
		"ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "csv=p=0",
		rel,
	}, ValidateOpts{Workspace: workspace})
	if err != nil {
		return 0, err
	}
	res, err := Run(ctx, bins, cmd, workspace, 8*time.Second)
	if err != nil {
		return 0, err
	}
	raw := strings.TrimSpace(res.Stdout)
	if i := strings.IndexAny(raw, "\r\n,"); i >= 0 {
		raw = strings.TrimSpace(raw[:i])
	}
	if raw == "" || strings.EqualFold(raw, "N/A") {
		return 0, nil
	}
	return strconv.ParseFloat(raw, 64)
}
