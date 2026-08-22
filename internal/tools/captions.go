package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"parallax/internal/ffmpeg"
	"parallax/internal/llm"
	"parallax/internal/projects"
	"parallax/internal/transcript"
)

func (e TranscriptEnv) registerCaptions(reg *Registry) {
	reg.Register(llm.NewFunctionTool(
		"add_captions",
		"Put timed captions on the timeline from the stored transcript so they appear in the program monitor and on sequence export. Use this instead of writing SRT, remuxing mov_text, or inventing a subtitles= ffmpeg filter. language: original (spoken language), en, or another language name/code such as hi, hindi, es, ja. style: soft (default — visible C1 caption track) or burn (drawn into the picture). Requires the file to be transcribed first.",
		json.RawMessage(`{
			"type":"object",
			"properties":{
				"path":{"type":"string","description":"Workspace video path such as media/talk.mp4"},
				"language":{"type":"string","description":"original, en, or a language name/code such as hi, hindi, es, ja"},
				"style":{"type":"string","enum":["soft","burn"],"description":"soft places a visible C1 caption track; burn draws captions into the video picture"},
				"apply_to":{"type":"string","description":"Only used with style burn. File to update in place. Omit to update path. Set none to keep a new burned file."}
			},
			"required":["path"]
		}`),
	), e.addCaptions)
}

func (e TranscriptEnv) addCaptions(ctx context.Context, raw json.RawMessage) Result {
	if e.Indexer == nil {
		return Result{OK: false, Error: "transcripts are not configured"}
	}
	if strings.TrimSpace(e.Workspace) == "" {
		return Result{OK: false, Error: "workspace is unavailable"}
	}
	var in struct {
		Path     string `json:"path"`
		Language string `json:"language"`
		Style    string `json:"style"`
		ApplyTo  string `json:"apply_to"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	rel := filepath.ToSlash(strings.TrimSpace(in.Path))
	if rel == "" {
		return Result{OK: false, Error: "path is required"}
	}
	doc, err := e.Indexer.Get(e.ProjectID, rel)
	if err != nil {
		return Result{OK: false, Error: "no transcript yet for " + rel + " — wait for indexing or upload audio first: " + err.Error()}
	}
	cues, mode, err := transcript.CaptionCues(doc, in.Language)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if mode != "original" && mode != "en" {
		var completer llm.Completer
		if e.Indexer.Completer != nil {
			completer = e.Indexer.Completer()
		}
		if err := transcript.TranslateCues(ctx, completer, cues, mode); err != nil {
			return Result{OK: false, Error: err.Error()}
		}
	}

	style := strings.ToLower(strings.TrimSpace(in.Style))
	if style == "" {
		style = "soft"
	}
	if style != "soft" && style != "burn" {
		return Result{OK: false, Error: "style must be soft or burn"}
	}

	langTag := mode
	if langTag == "original" {
		langTag = firstNonEmpty(transcript.NormalizeCaptionLang(doc.Language), "und")
	}
	langTag = transcript.NormalizeCaptionLang(langTag)
	if langTag == "" {
		langTag = "und"
	}
	label := transcript.CaptionLanguageName(langTag) + " captions"
	srtRel := captionSRTRel(rel, langTag)
	scratchSRT := filepath.ToSlash(filepath.Join(".scratch", "captions.srt"))
	srtBody := transcript.WriteSRT(cues)
	if err := writeWorkspaceFile(e.Workspace, srtRel, srtBody); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if err := writeWorkspaceFile(e.Workspace, scratchSRT, srtBody); err != nil {
		return Result{OK: false, Error: err.Error()}
	}

	if style == "soft" {
		return e.placeSoftCaptions(rel, srtRel, langTag, label, cues)
	}
	return e.burnCaptions(ctx, in.ApplyTo, rel, srtRel, scratchSRT, langTag, label, cues)
}

func (e TranscriptEnv) placeSoftCaptions(videoRel, srtRel, langTag, label string, cues []transcript.Cue) Result {
	if e.Transaction == nil {
		return Result{OK: false, Error: "timeline is unavailable; cannot place visible captions"}
	}
	timeline := e.Transaction.Get()
	ops, created, playhead := captionTimelineOps(timeline, videoRel, srtRel, langTag, label, cues)
	if len(ops) == 0 {
		return Result{OK: false, Error: "could not place captions on the timeline"}
	}
	result, err := e.Transaction.Apply(ops)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	focusID := ""
	if len(created) > 0 {
		focusID = created[0]
	} else if len(result.CreatedIDs) > 0 {
		focusID = result.CreatedIDs[0]
	}
	if focusID != "" {
		e.Transaction.Focus(focusID, playhead)
	}
	return Result{OK: true, Output: map[string]any{
		"path":        videoRel,
		"applied_to":  videoRel,
		"srt":         srtRel,
		"language":    langTag,
		"style":       "soft",
		"cues":        len(cues),
		"track":       "C1",
		"created_ids": result.CreatedIDs,
		"removed_ids": result.RemovedIDs,
		"timeline":    e.Transaction.Get(),
		"in_place":    false,
		"visible":     true,
		"note":        "Captions are on track C1 and show in the program monitor and sequence export. Do not remux a mov_text stream — the HTML preview cannot display it.",
	}}
}

func captionTimelineOps(doc projects.Timeline, videoRel, srtRel, langTag, label string, cues []transcript.Cue) ([]projects.TimelineOperation, []string, int) {
	fps := doc.FPS
	if fps < 1 {
		fps = 24
	}
	var remove []string
	for _, clip := range doc.Clips {
		if isCaptionFor(clip, videoRel, langTag) {
			remove = append(remove, clip.ID)
		}
	}
	ops := make([]projects.TimelineOperation, 0, 8)
	if len(remove) > 0 {
		ops = append(ops, projects.TimelineOperation{Type: "remove_items", IDs: remove})
	}

	videos := videoClipsFor(doc, videoRel)
	if len(videos) == 0 {
		duration := captionSourceFrames(cues, fps)
		videos = []projects.TimelineClip{{
			Name:                 label,
			Track:                "V1",
			Kind:                 "video",
			StartFrame:           0,
			DurationFrames:       duration,
			SourceInFrame:        0,
			SourceDurationFrames: duration,
			MediaPath:            videoRel,
		}}
	}

	created := make([]string, 0, len(videos))
	playhead := 0
	firstCue := 0.0
	if len(cues) > 0 {
		firstCue = cues[0].Start
	}
	for i, video := range videos {
		linkID := projects.EnsureLinkID(&doc, video.ID)
		if video.ID != "" && linkID != "" && video.LinkID != linkID {
			updated := video
			updated.LinkID = linkID
			ops = append(ops, projects.TimelineOperation{Type: "update_item", Item: &updated})
			for _, other := range doc.Clips {
				if other.ID == video.ID || other.LinkID != linkID || other.Kind == "caption" {
					continue
				}
				if other.Kind == "audio" && other.MediaPath == video.MediaPath {
					audio := other
					ops = append(ops, projects.TimelineOperation{Type: "update_item", Item: &audio})
				}
			}
			video.LinkID = linkID
		}
		item := projects.NewCaptionClip(video, srtRel, langTag, label)
		ops = append(ops, projects.TimelineOperation{Type: "add_item", Item: &item})
		created = append(created, item.ID)
		if i == 0 {
			frame := video.StartFrame + projects.SecondsToFrames(firstCue, fps) - video.SourceInFrame
			if frame < video.StartFrame {
				frame = video.StartFrame
			}
			if frame >= video.StartFrame+video.DurationFrames {
				frame = video.StartFrame
			}
			playhead = frame
		}
	}
	return ops, created, playhead
}

func videoClipsFor(doc projects.Timeline, videoRel string) []projects.TimelineClip {
	want := filepath.ToSlash(strings.TrimSpace(videoRel))
	var out []projects.TimelineClip
	for _, clip := range doc.Clips {
		if clip.Kind != "video" {
			continue
		}
		if filepath.ToSlash(clip.MediaPath) == want {
			out = append(out, clip)
		}
	}
	return out
}

func isCaptionFor(clip projects.TimelineClip, videoRel, _ string) bool {
	if clip.Kind != "caption" && clip.Track != "C1" {
		return false
	}
	want := filepath.ToSlash(strings.TrimSpace(videoRel))
	if clip.Captions != nil && filepath.ToSlash(clip.Captions.Source) != "" {
		return filepath.ToSlash(clip.Captions.Source) == want
	}
	return false
}

func captionSourceFrames(cues []transcript.Cue, fps int) int {
	end := 0.0
	for _, cue := range cues {
		if cue.End > end {
			end = cue.End
		}
	}
	if end <= 0 {
		end = 5
	}
	return projects.SecondsToFrames(end, fps)
}

func captionSRTRel(videoRel, langTag string) string {
	base := strings.TrimSuffix(filepath.Base(videoRel), filepath.Ext(videoRel))
	stem := safeFileStem(base)
	return filepath.ToSlash(filepath.Join(".parallax", "captions", stem+"."+safeLangFile(langTag)+".srt"))
}

func (e TranscriptEnv) burnCaptions(ctx context.Context, applyTo, rel, srtRel, scratchSRT, langTag, label string, cues []transcript.Cue) Result {
	apply := strings.TrimSpace(applyTo)
	if apply == "" {
		apply = rel
	}
	keepNew := strings.EqualFold(apply, "none") || apply == "-"
	ext := strings.ToLower(filepath.Ext(rel))
	if ext == "" {
		ext = ".mp4"
	}
	base := strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
	filter, err := ffmpeg.SubtitleFilter(e.Workspace, scratchSRT, langTag)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	args := []string{"-y", "-i", rel, "-vf", filter, "-c:v", "libx264", "-c:a", "copy"}
	outRel := apply
	if !keepNew {
		outRel = filepath.ToSlash(filepath.Join(".scratch", fmt.Sprintf("caption-%d%s", time.Now().UnixNano(), ext)))
	} else {
		outRel = filepath.ToSlash(filepath.Join(filepath.Dir(rel), base+"."+safeLangFile(langTag)+".captioned"+ext))
	}
	runArgs := append(append([]string{}, args...), outRel)
	cmd, err := ffmpeg.Validate(runArgs, ffmpeg.ValidateOpts{Workspace: e.Workspace})
	if err != nil {
		return Result{OK: false, Error: "invalid caption command: " + err.Error()}
	}
	res, err := ffmpeg.Run(ctx, e.Bins, cmd, e.Workspace, 15*time.Minute)
	if err != nil {
		return Result{OK: false, Error: err.Error(), Output: map[string]any{
			"stderr":   trimOutput(res.Stderr, 12<<10),
			"srt":      srtRel,
			"language": langTag,
			"style":    "burn",
		}}
	}
	applied := outRel
	if !keepNew {
		if err := replaceWorkspaceFile(e.Workspace, outRel, apply); err != nil {
			return Result{OK: false, Error: "applied captions failed: " + err.Error()}
		}
		applied = apply
		if e.OnMutation != nil {
			e.OnMutation()
		}
		if e.OnApplied != nil {
			e.OnApplied(apply)
		}
	} else if e.OnMutation != nil {
		e.OnMutation()
	}
	return Result{OK: true, Output: map[string]any{
		"path":       rel,
		"applied_to": applied,
		"srt":        srtRel,
		"language":   langTag,
		"style":      "burn",
		"cues":       len(cues),
		"label":      label,
		"in_place":   !keepNew,
		"visible":    true,
		"note":       "Captions are burned into the picture. Play the video to see them. Prefer style soft unless the user asked to burn them in.",
	}}
}

func writeWorkspaceFile(workspace, rel, body string) error {
	abs, err := ffmpeg.ResolveInWorkspace(workspace, rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	return os.WriteFile(abs, []byte(body), 0o644)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func safeFileStem(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "captions"
	}
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "captions"
	}
	if len(s) > 48 {
		s = s[:48]
	}
	return strings.Trim(s, "-")
}

func safeLangFile(lang string) string {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if lang == "" {
		return "und"
	}
	var b strings.Builder
	for _, r := range lang {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "und"
	}
	s := b.String()
	if len(s) > 8 {
		s = s[:8]
	}
	return s
}
