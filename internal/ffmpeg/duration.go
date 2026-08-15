package ffmpeg

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// MediaProbe is duration plus coded frame size from ffprobe.
type MediaProbe struct {
	Duration float64
	Width    int
	Height   int
}

// ProbeDuration returns the container duration in seconds for a workspace file.
func ProbeDuration(ctx context.Context, bins Bins, workspace, rel string) (float64, error) {
	info, err := ProbeMedia(ctx, bins, workspace, rel)
	if err != nil {
		return 0, err
	}
	return info.Duration, nil
}

// ProbeMedia returns duration and the first video/image stream size.
func ProbeMedia(ctx context.Context, bins Bins, workspace, rel string) (MediaProbe, error) {
	cmd, err := Validate([]string{
		"ffprobe",
		"-v", "error",
		"-show_entries", "format=duration:stream=width,height,codec_type",
		"-of", "json",
		rel,
	}, ValidateOpts{Workspace: workspace})
	if err != nil {
		return MediaProbe{}, err
	}
	res, err := Run(ctx, bins, cmd, workspace, 8*time.Second)
	if err != nil {
		return MediaProbe{}, err
	}
	return ParseMediaProbe(res.Stdout)
}

// ParseMediaProbe reads duration and the first video/image frame size from ffprobe JSON.
func ParseMediaProbe(raw string) (MediaProbe, error) {
	var payload struct {
		Streams []struct {
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			CodecType string `json:"codec_type"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &payload); err != nil {
		return MediaProbe{}, err
	}
	var out MediaProbe
	if d := strings.TrimSpace(payload.Format.Duration); d != "" && !strings.EqualFold(d, "N/A") {
		if parsed, err := strconv.ParseFloat(d, 64); err == nil && parsed > 0 {
			out.Duration = parsed
		}
	}
	for _, stream := range payload.Streams {
		if stream.Width < 1 || stream.Height < 1 {
			continue
		}
		if stream.CodecType == "audio" {
			continue
		}
		out.Width = stream.Width
		out.Height = stream.Height
		break
	}
	return out, nil
}
