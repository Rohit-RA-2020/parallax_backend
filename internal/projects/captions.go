package projects

import "strings"

// NewCaptionClip builds a C1 caption track aligned with a video clip.
// Cue times in the referenced SRT are source-media seconds.
func NewCaptionClip(video TimelineClip, srtRel, language, name string) TimelineClip {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Captions"
	}
	linkID := strings.TrimSpace(video.LinkID)
	source := strings.TrimSpace(video.MediaPath)
	return TimelineClip{
		ID:                   newTimelineID("clip"),
		Name:                 name,
		Track:                "C1",
		Kind:                 "caption",
		StartFrame:           video.StartFrame,
		DurationFrames:       video.DurationFrames,
		SourceInFrame:        video.SourceInFrame,
		SourceDurationFrames: video.SourceDurationFrames,
		MediaPath:            srtRel,
		MediaType:            "subtitle",
		Color:                colorCaption,
		LinkID:               linkID,
		Transform:            &TimelineTransform{X: 960, Y: 1000, AnchorX: .5, AnchorY: 1, ScaleX: 1, ScaleY: 1, Opacity: 1},
		Title:                &TimelineTitle{Text: name, FontSize: 32, FontWeight: 600, Align: "center", Fill: "#ffffff"},
		Captions:             &TimelineCaptions{Language: strings.TrimSpace(language), Source: source},
	}
}

// EnsureLinkID gives a video/audio pair a shared link so a new caption clip
// can ride along with trims and moves.
func EnsureLinkID(doc *Timeline, videoID string) string {
	if doc == nil {
		return ""
	}
	idx := timelineClipIndex(doc.Clips, videoID)
	if idx < 0 {
		return ""
	}
	if id := strings.TrimSpace(doc.Clips[idx].LinkID); id != "" {
		return id
	}
	linkID := newTimelineID("link")
	doc.Clips[idx].LinkID = linkID
	video := doc.Clips[idx]
	for i := range doc.Clips {
		if i == idx {
			continue
		}
		other := doc.Clips[i]
		if other.Kind != "audio" {
			continue
		}
		if other.LinkID != "" {
			continue
		}
		if other.MediaPath == video.MediaPath && other.StartFrame == video.StartFrame {
			doc.Clips[i].LinkID = linkID
		}
	}
	return linkID
}
