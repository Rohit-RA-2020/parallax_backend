package ffmpeg

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultSceneThreshold = 0.3
	sceneDetectTimeout    = 8 * time.Minute
	frameExtractTimeout   = 45 * time.Second
)

// DetectScenes returns timestamps (seconds) where the picture changes.
// The first frame of the file is not included; callers treat 0 as the first shot start.
func DetectScenes(ctx context.Context, bins Bins, workspace, rel string, threshold float64) ([]float64, error) {
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if rel == "" {
		return nil, fmt.Errorf("media path is required")
	}
	if threshold <= 0 || threshold >= 1 {
		threshold = DefaultSceneThreshold
	}
	if _, err := ResolveInWorkspace(workspace, rel); err != nil {
		return nil, err
	}
	expr := fmt.Sprintf("movie=%s,select=gt(scene\\,%.3f)", escapeLavfiMoviePath(rel), threshold)
	cmd, err := Validate([]string{
		"ffprobe",
		"-v", "error",
		"-f", "lavfi",
		"-i", expr,
		"-show_frames",
		"-show_entries", "frame=pts_time,pkt_pts_time,best_effort_timestamp_time",
		"-of", "json",
	}, ValidateOpts{Workspace: workspace})
	if err != nil {
		return nil, fmt.Errorf("detect scenes: %w", err)
	}
	res, err := Run(ctx, bins, cmd, workspace, sceneDetectTimeout)
	if err != nil {
		return nil, fmt.Errorf("detect scenes: %w", err)
	}
	return ParseSceneTimes(res.Stdout)
}

// ParseSceneTimes reads pts values from an ffprobe -show_frames JSON dump.
func ParseSceneTimes(raw string) ([]float64, error) {
	var payload struct {
		Frames []struct {
			PtsTime    string `json:"pts_time"`
			PktPtsTime string `json:"pkt_pts_time"`
			BestEffort string `json:"best_effort_timestamp_time"`
		} `json:"frames"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &payload); err != nil {
		return nil, fmt.Errorf("detect scenes: decode frames: %w", err)
	}
	var out []float64
	seen := map[string]bool{}
	for _, frame := range payload.Frames {
		sec, ok := parseTime(frame.PtsTime, frame.BestEffort, frame.PktPtsTime)
		if !ok {
			continue
		}
		key := strconv.FormatFloat(sec, 'f', 3, 64)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, sec)
	}
	return out, nil
}

func parseTime(vals ...string) (float64, bool) {
	for _, val := range vals {
		val = strings.TrimSpace(val)
		if val == "" || strings.EqualFold(val, "N/A") {
			continue
		}
		sec, err := strconv.ParseFloat(val, 64)
		if err != nil || sec < 0 {
			continue
		}
		return sec, true
	}
	return 0, false
}

func escapeLavfiMoviePath(rel string) string {
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	r := strings.NewReplacer(`\`, `\\`, `:`, `\:`, `'`, `\'`, `,`, `\,`, `[`, `\[`, `]`, `\]`, `;`, `\;`)
	return r.Replace(rel)
}

// ExtractFrame writes one JPEG still at the given timestamp (seconds).
func ExtractFrame(ctx context.Context, bins Bins, workspace, inRel, outRel string, at float64) error {
	if at < 0 {
		at = 0
	}
	if err := os.MkdirAll(filepath.Join(workspace, filepath.Dir(filepath.FromSlash(outRel))), 0o755); err != nil {
		return err
	}
	cmd, err := Validate([]string{
		"ffmpeg", "-y",
		"-ss", formatTimestamp(at),
		"-i", inRel,
		"-frames:v", "1",
		"-q:v", "3",
		outRel,
	}, ValidateOpts{Workspace: workspace})
	if err != nil {
		return fmt.Errorf("extract frame: %w", err)
	}
	if _, err := Run(ctx, bins, cmd, workspace, frameExtractTimeout); err != nil {
		return fmt.Errorf("extract frame: %w", err)
	}
	return nil
}

func formatTimestamp(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	return strconv.FormatFloat(sec, 'f', 3, 64)
}
