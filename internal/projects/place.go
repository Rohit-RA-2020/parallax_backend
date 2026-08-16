package projects

import (
	"math"
	"path/filepath"
	"strings"
)

const (
	colorVideo            = "#8a6a48"
	colorAudio            = "#3d8f72"
	defaultStillSeconds   = 5
	defaultUnknownSeconds = 5
)

// MediaLayout is enough information to place a file the way the editor does:
// picture on V1, linked sound on A1, stills as a short V1 hold.
type MediaLayout struct {
	Path                 string
	Name                 string
	StartFrame           int
	DurationFrames       int
	SourceDurationFrames int
	HasPicture           bool
	HasAudio             bool
	IsImage              bool
}

// SecondsToFrames converts a media duration into timeline frames.
func SecondsToFrames(seconds float64, fps int) int {
	if fps < 1 {
		fps = timelineDefaultFPS
	}
	if seconds <= 0 {
		return 0
	}
	frames := int(math.Round(seconds * float64(fps)))
	if frames < 1 {
		return 1
	}
	return frames
}

// TimelineEndFrame is the first free frame after the last clip.
func TimelineEndFrame(doc Timeline) int {
	end := 0
	for _, clip := range doc.Clips {
		if next := clip.StartFrame + clip.DurationFrames; next > end {
			end = next
		}
	}
	return end
}

// PlaceMediaClips builds the V1/A1 pair (or a single track) for one imported file.
func PlaceMediaClips(layout MediaLayout) []TimelineClip {
	path, err := sanitizeMediaPath(layout.Path)
	if err != nil || path == "" {
		return nil
	}
	name := strings.TrimSpace(layout.Name)
	if name == "" {
		name = mediaDisplayName(path)
	}
	duration := layout.DurationFrames
	if duration < 1 {
		duration = 1
	}
	source := layout.SourceDurationFrames
	if source < 1 {
		source = duration
	}
	start := layout.StartFrame
	if start < 0 {
		start = 0
	}

	hasPicture := layout.HasPicture || layout.IsImage
	hasAudio := layout.HasAudio && !layout.IsImage
	if !hasPicture && !hasAudio {
		hasPicture = true
	}

	mediaType := "video"
	if layout.IsImage {
		mediaType = "image"
	} else if !hasPicture {
		mediaType = "audio"
	}

	linkID := ""
	if hasPicture && hasAudio {
		linkID = newTimelineID("link")
	}

	out := make([]TimelineClip, 0, 2)
	if hasPicture {
		out = append(out, TimelineClip{
			ID:                   newTimelineID("clip"),
			Name:                 name,
			Track:                "V1",
			Kind:                 "video",
			StartFrame:           start,
			DurationFrames:       duration,
			SourceInFrame:        0,
			SourceDurationFrames: source,
			MediaPath:            path,
			MediaType:            mediaType,
			Color:                colorVideo,
			LinkID:               linkID,
		})
	}
	if hasAudio {
		audioType := mediaType
		if audioType == "image" {
			audioType = "audio"
		}
		out = append(out, TimelineClip{
			ID:                   newTimelineID("clip"),
			Name:                 name,
			Track:                "A1",
			Kind:                 "audio",
			StartFrame:           start,
			DurationFrames:       duration,
			SourceInFrame:        0,
			SourceDurationFrames: source,
			MediaPath:            path,
			MediaType:            audioType,
			Color:                colorAudio,
			WaveSeed:             waveSeed(path),
			LinkID:               linkID,
		})
	}
	return out
}

func mediaDisplayName(path string) string {
	base := filepath.Base(filepath.ToSlash(path))
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	base = strings.TrimSpace(base)
	if base == "" {
		return "Clip"
	}
	return base
}

func waveSeed(path string) int {
	seed := 0
	for _, r := range path {
		seed = (seed*31 + int(r)) % 200
	}
	return seed + 1
}

func LooksLikeSecondsAsFrames(frames int, sourceSeconds float64, fps int) bool {
	if frames < 1 || sourceSeconds <= 0 || fps < 1 {
		return false
	}
	sourceFrames := SecondsToFrames(sourceSeconds, fps)
	if sourceFrames <= frames {
		return false
	}
	// Shorter than one second of timeline, while the file is at least a second:
	// almost always seconds-as-frames or the 1-frame schema minimum.
	if frames < fps && sourceFrames >= fps {
		return true
	}
	return math.Abs(float64(frames)-sourceSeconds) < 0.6
}
