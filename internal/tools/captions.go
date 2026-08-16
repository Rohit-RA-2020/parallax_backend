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
	"parallax/internal/transcript"
)

func (e TranscriptEnv) registerCaptions(reg *Registry) {
	reg.Register(llm.NewFunctionTool(
		"add_captions",
		"Add timed captions to a video from the stored transcript. Use this instead of writing SRT or ffmpeg subtitle filters by hand. language: original (spoken language), en, or another language code. style: soft (default, movable subtitle track) or burn (drawn into the picture). Requires the file to be transcribed first.",
		json.RawMessage(`{
			"type":"object",
			"properties":{
				"path":{"type":"string","description":"Workspace video path such as media/talk.mp4"},
				"language":{"type":"string","description":"original, en, or a language name/code such as hi, es, ja"},
				"style":{"type":"string","enum":["soft","burn"],"description":"soft remuxes a subtitle track; burn draws captions into the video"},
				"apply_to":{"type":"string","description":"File to update in place. Omit to update path. Set none to keep a new file plus the .srt."}
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
		langTag = firstNonEmpty(doc.Language, "und")
	}
	base := strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
	srtRel := filepath.ToSlash(filepath.Join(filepath.Dir(rel), base+"."+safeLangFile(langTag)+".srt"))
	scratchSRT := filepath.ToSlash(filepath.Join(".scratch", "captions.srt"))
	srtBody := transcript.WriteSRT(cues)
	if err := writeWorkspaceFile(e.Workspace, srtRel, srtBody); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if err := writeWorkspaceFile(e.Workspace, scratchSRT, srtBody); err != nil {
		return Result{OK: false, Error: err.Error()}
	}

	applyTo := strings.TrimSpace(in.ApplyTo)
	if applyTo == "" {
		applyTo = rel
	}
	keepNew := strings.EqualFold(applyTo, "none") || applyTo == "-"
	ext := strings.ToLower(filepath.Ext(rel))
	if ext == "" {
		ext = ".mp4"
	}

	var args []string
	switch style {
	case "burn":
		args = []string{"-y", "-i", rel, "-vf", "subtitles=" + scratchSRT, "-c:v", "libx264", "-c:a", "copy"}
	default:
		if ext == ".webm" || ext == ".gif" {
			return Result{OK: false, Error: "soft captions are not supported on " + ext + "; use style burn"}
		}
		codec := "mov_text"
		if ext == ".mkv" {
			codec = "srt"
		}
		args = []string{
			"-y", "-i", rel, "-i", scratchSRT,
			"-map", "0", "-map", "1",
			"-c", "copy", "-c:s", codec,
			"-metadata:s:s:0", "language=" + safeLangFile(langTag),
		}
	}

	outRel := applyTo
	runArgs := args
	if !keepNew {
		outRel = filepath.ToSlash(filepath.Join(".scratch", fmt.Sprintf("caption-%d%s", time.Now().UnixNano(), ext)))
		runArgs = append(append([]string{}, args...), outRel)
	} else {
		outRel = filepath.ToSlash(filepath.Join(filepath.Dir(rel), base+"."+safeLangFile(langTag)+".captioned"+ext))
		runArgs = append(append([]string{}, args...), outRel)
	}

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
			"style":    style,
		}}
	}
	applied := outRel
	if !keepNew {
		if err := replaceWorkspaceFile(e.Workspace, outRel, applyTo); err != nil {
			return Result{OK: false, Error: "applied captions failed: " + err.Error()}
		}
		applied = applyTo
		if e.OnMutation != nil {
			e.OnMutation()
		}
		if e.OnApplied != nil {
			e.OnApplied(applyTo)
		}
	} else if e.OnMutation != nil {
		e.OnMutation()
	}

	return Result{OK: true, Output: map[string]any{
		"path":       rel,
		"applied_to": applied,
		"srt":        srtRel,
		"language":   langTag,
		"style":      style,
		"cues":       len(cues),
		"in_place":   !keepNew,
		"note":       "Captions come from the stored transcript timings. Soft keeps a subtitle track; burn draws them into the picture.",
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
