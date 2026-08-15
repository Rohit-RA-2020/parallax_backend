package ffmpeg

import (
	"fmt"
	"strconv"
	"strings"
)

// ExportSpec is a structured render request. Paths are workspace-relative.
type ExportSpec struct {
	Source     string
	Format     string
	Quality    string
	Resolution string
	FPS        int
	Audio      bool
	Start      float64
	Duration   float64
}

const SequenceSource = "sequence"

func (s ExportSpec) IsSequence() bool {
	return strings.EqualFold(strings.TrimSpace(s.Source), SequenceSource)
}

func (s *ExportSpec) Normalize() error {
	s.Source = strings.TrimSpace(s.Source)
	if s.Source == "" {
		return fmt.Errorf("source is required")
	}
	s.Format = strings.ToLower(strings.TrimSpace(s.Format))
	if s.Format == "" {
		s.Format = "mp4"
	}
	switch s.Format {
	case "mp4", "mov", "webm", "gif", "mp3":
	default:
		return fmt.Errorf("unsupported format %q", s.Format)
	}
	s.Quality = strings.ToLower(strings.TrimSpace(s.Quality))
	if s.Quality == "" {
		s.Quality = "standard"
	}
	switch s.Quality {
	case "draft", "standard", "high", "original":
	default:
		return fmt.Errorf("unsupported quality %q", s.Quality)
	}
	s.Resolution = strings.ToLower(strings.TrimSpace(s.Resolution))
	if s.Resolution == "" || s.Resolution == "original" {
		s.Resolution = "source"
	}
	switch s.Resolution {
	case "source", "3840x2160", "1920x1080", "1280x720", "854x480":
	default:
		return fmt.Errorf("unsupported resolution %q", s.Resolution)
	}
	if s.FPS < 0 || s.FPS > 120 {
		return fmt.Errorf("frame rate must be between 0 and 120")
	}
	if s.Start < 0 {
		return fmt.Errorf("start cannot be negative")
	}
	if s.Duration < 0 {
		return fmt.Errorf("duration cannot be negative")
	}
	if s.Format == "gif" || s.Format == "mp3" {
		s.Audio = s.Format == "mp3"
	}
	if s.Format == "gif" && s.FPS == 0 {
		s.FPS = 12
	}
	return nil
}

func (s ExportSpec) Ext() string {
	return "." + s.Format
}

func (s ExportSpec) copyStreams() bool {
	if s.IsSequence() {
		return false
	}
	if s.Format != "mp4" && s.Format != "mov" {
		return false
	}
	if s.Quality != "original" {
		return false
	}
	if s.Resolution != "source" || s.FPS > 0 {
		return false
	}
	return true
}

// BuildExportArgs returns ffmpeg argv without the binary.
func BuildExportArgs(spec ExportSpec, dest string) ([]string, error) {
	if err := spec.Normalize(); err != nil {
		return nil, err
	}
	dest = strings.TrimSpace(dest)
	if dest == "" {
		return nil, fmt.Errorf("export destination is required")
	}

	args := []string{"-y", "-hide_banner"}
	if spec.Start > 0 {
		args = append(args, "-ss", formatSeconds(spec.Start))
	}
	args = append(args, "-i", spec.Source)
	if spec.Duration > 0 {
		args = append(args, "-t", formatSeconds(spec.Duration))
	}

	if spec.Format == "mp3" {
		args = append(args, "-vn", "-c:a", "libmp3lame", "-q:a", mp3Quality(spec.Quality), dest)
		return args, nil
	}
	if spec.Format == "gif" {
		vf := gifFilter(spec)
		args = append(args, "-vf", vf, "-an", "-loop", "0", dest)
		return args, nil
	}
	if spec.copyStreams() {
		if spec.Audio {
			args = append(args, "-c", "copy", dest)
		} else {
			args = append(args, "-c:v", "copy", "-an", dest)
		}
		return args, nil
	}

	if vf := videoFilter(spec); vf != "" {
		args = append(args, "-vf", vf)
	}
	if spec.FPS > 0 && spec.Resolution == "source" {
		args = append(args, "-r", strconv.Itoa(spec.FPS))
	}

	switch spec.Format {
	case "webm":
		args = append(args, "-c:v", "libvpx-vp9", "-b:v", "0", "-crf", vp9CRF(spec.Quality), "-pix_fmt", "yuv420p")
		if spec.Audio {
			args = append(args, "-c:a", "libopus", "-b:a", "128k")
		} else {
			args = append(args, "-an")
		}
	default:
		preset, crf := x264Quality(spec.Quality)
		args = append(args, "-c:v", "libx264", "-preset", preset, "-crf", crf, "-pix_fmt", "yuv420p")
		if spec.Audio {
			args = append(args, "-c:a", "aac", "-b:a", "192k")
		} else {
			args = append(args, "-an")
		}
	}
	args = append(args, dest)
	return args, nil
}

func videoFilter(spec ExportSpec) string {
	var parts []string
	if w, h, ok := parseSize(spec.Resolution); ok {
		parts = append(parts, fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease:force_divisible_by=2,pad=%d:%d:(ow-iw)/2:(oh-ih)/2", w, h, w, h))
	}
	if spec.FPS > 0 && spec.Resolution != "source" {
		parts = append(parts, fmt.Sprintf("fps=%d", spec.FPS))
	}
	return strings.Join(parts, ",")
}

func gifFilter(spec ExportSpec) string {
	fps := spec.FPS
	if fps <= 0 {
		fps = 12
	}
	scale := "scale=480:-2:flags=lanczos"
	if w, h, ok := parseSize(spec.Resolution); ok {
		scale = fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease:flags=lanczos:force_divisible_by=2", w, h)
	} else if spec.Resolution == "source" {
		scale = "scale=iw:-2:flags=lanczos:force_divisible_by=2"
	}
	return fmt.Sprintf("fps=%d,%s", fps, scale)
}

func parseSize(res string) (int, int, bool) {
	switch res {
	case "3840x2160":
		return 3840, 2160, true
	case "1920x1080":
		return 1920, 1080, true
	case "1280x720":
		return 1280, 720, true
	case "854x480":
		return 854, 480, true
	}
	return 0, 0, false
}

func x264Quality(q string) (preset, crf string) {
	switch q {
	case "draft":
		return "ultrafast", "28"
	case "high":
		return "slow", "16"
	default:
		return "veryfast", "20"
	}
}

func vp9CRF(q string) string {
	switch q {
	case "draft":
		return "40"
	case "high":
		return "28"
	default:
		return "33"
	}
}

func mp3Quality(q string) string {
	switch q {
	case "draft":
		return "5"
	case "high":
		return "0"
	default:
		return "2"
	}
}

func formatSeconds(v float64) string {
	s := strconv.FormatFloat(v, 'f', 3, 64)
	return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}
