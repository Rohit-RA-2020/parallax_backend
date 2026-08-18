package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"parallax/internal/ffmpeg"
	"parallax/internal/gemini"
	"parallax/internal/llm"
)

const maxVideoInputBytes = 64 << 20

type VideoGenerationEnv struct {
	Workspace  string
	Bins       ffmpeg.Bins
	Client     *gemini.Client
	OmniModel  string
	VeoModel   string
	Poll       time.Duration
	OnMutation func()
	OnApplied  func(rel string)
}

func RegisterVideoGeneration(reg *Registry, env VideoGenerationEnv) {
	reg.Register(llm.NewFunctionTool(
		"generate_video",
		"Generate or edit a video with Gemini and save it into the project media bin. The tool automatically chooses Gemini Omni Flash for normal, fast, multimodal generation and conversational edits, or standard Veo 3.1 for cinematic/high-fidelity requests, explicit duration/resolution, reference-image control, first/last-frame interpolation, and extension. Provide task as text_to_video, image_to_video, reference_to_video, edit, extend, or interpolate when the intent is clear. Generated videos are indexed automatically; call place_media only when the user asks to put the result on the timeline.",
		json.RawMessage(`{
			"type":"object",
			"properties":{
				"prompt":{"type":"string","description":"Detailed scene, motion, camera, lighting, mood, dialogue, ambience, or edit instructions."},
				"task":{"type":"string","enum":["text_to_video","image_to_video","reference_to_video","edit","extend","interpolate"],"description":"Semantic generation task. Omit when obvious from the inputs."},
				"source":{"type":"string","description":"Project-relative image or video path to animate or edit."},
				"images":{"type":"array","items":{"type":"string"},"description":"Additional project-relative reference image paths. Veo supports up to 3."},
				"last_frame":{"type":"string","description":"Project-relative final image for Veo first/last-frame interpolation."},
				"previous_interaction_id":{"type":"string","description":"Gemini Omni interaction id for a conversational follow-up edit."},
				"aspect_ratio":{"type":"string","enum":["16:9","9:16"]},
				"duration_seconds":{"type":"integer","enum":[4,6,8],"description":"Veo duration. Supplying this routes to Veo."},
				"resolution":{"type":"string","enum":["720p","1080p","4k"],"description":"Output resolution. 1080p and 4k route to Veo and require 8 seconds."},
				"filename":{"type":"string","description":"Optional output basename; the file is always saved as MP4."},
				"apply_to":{"type":"string","description":"Omit to replace a source video during an edit; use none to keep a separate generated asset, or provide a project-relative video path to replace."}
			},
			"required":["prompt"]
		}`),
	), env.generateVideo)
}

func (e VideoGenerationEnv) generateVideo(ctx context.Context, raw json.RawMessage) Result {
	if e.Client == nil || strings.TrimSpace(e.Client.APIKey) == "" {
		return Result{OK: false, Error: "Gemini video is not configured; set GEMINI_API_KEY on the server"}
	}
	if strings.TrimSpace(e.Workspace) == "" {
		return Result{OK: false, Error: "project workspace is not configured"}
	}
	var in struct {
		Prompt                string   `json:"prompt"`
		Task                  string   `json:"task"`
		Source                string   `json:"source"`
		Images                []string `json:"images"`
		LastFrame             string   `json:"last_frame"`
		PreviousInteractionID string   `json:"previous_interaction_id"`
		AspectRatio           string   `json:"aspect_ratio"`
		DurationSeconds       int      `json:"duration_seconds"`
		Resolution            string   `json:"resolution"`
		Filename              string   `json:"filename"`
		ApplyTo               string   `json:"apply_to"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	in.Prompt = strings.TrimSpace(in.Prompt)
	if in.Prompt == "" {
		return Result{OK: false, Error: "prompt is required"}
	}

	source := strings.TrimSpace(in.Source)
	task := strings.ToLower(strings.TrimSpace(in.Task))
	if task == "" {
		switch {
		case strings.TrimSpace(in.PreviousInteractionID) != "":
			task = "edit"
		case strings.TrimSpace(in.LastFrame) != "":
			task = "interpolate"
		case len(in.Images) > 0:
			task = "reference_to_video"
		case source != "":
			task = "image_to_video"
		default:
			task = "text_to_video"
		}
	}
	if !validVideoTask(task) {
		return Result{OK: false, Error: "task must be text_to_video, image_to_video, reference_to_video, edit, extend, or interpolate"}
	}

	var sourceImage, sourceVideo *gemini.VideoPart
	if source != "" {
		part, kind, err := loadVideoInput(e.Workspace, source)
		if err != nil {
			return Result{OK: false, Error: err.Error()}
		}
		if kind == "image" {
			sourceImage = &part
		} else {
			sourceVideo = &part
		}
	}
	if strings.TrimSpace(in.Task) == "" && sourceVideo != nil {
		task = "edit"
	}
	refs := make([]gemini.VideoPart, 0, len(in.Images))
	for _, rawPath := range in.Images {
		part, kind, err := loadVideoInput(e.Workspace, rawPath)
		if err != nil {
			return Result{OK: false, Error: err.Error()}
		}
		if kind != "image" {
			return Result{OK: false, Error: "images must contain project-relative image files"}
		}
		refs = append(refs, part)
	}
	var lastFrame *gemini.VideoPart
	if strings.TrimSpace(in.LastFrame) != "" {
		part, kind, err := loadVideoInput(e.Workspace, in.LastFrame)
		if err != nil {
			return Result{OK: false, Error: err.Error()}
		}
		if kind != "image" {
			return Result{OK: false, Error: "last_frame must be a project-relative image file"}
		}
		lastFrame = &part
	}

	provider, err := chooseVideoProvider(task, in.PreviousInteractionID, sourceVideo != nil, lastFrame != nil, len(refs), in.DurationSeconds, in.Resolution)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	request := gemini.VideoRequest{
		Prompt: in.Prompt, Task: task, SourceImage: sourceImage, SourceVideo: sourceVideo,
		ReferenceImages: refs, LastFrame: lastFrame, PreviousInteractionID: strings.TrimSpace(in.PreviousInteractionID),
		AspectRatio: strings.TrimSpace(in.AspectRatio), DurationSeconds: in.DurationSeconds,
		Resolution: strings.TrimSpace(in.Resolution), PollInterval: e.Poll,
	}
	var generated gemini.VideoResult
	if provider == "omni" {
		request.Model = firstNonEmpty(e.OmniModel, gemini.DefaultOmniVideoModel)
		generated, err = e.Client.GenerateOmni(ctx, request)
	} else {
		request.Model = firstNonEmpty(e.VeoModel, gemini.DefaultVeoVideoModel)
		generated, err = e.Client.GenerateVeo(ctx, request)
	}
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}

	applyTo := strings.TrimSpace(in.ApplyTo)
	if applyTo == "" && sourceVideo != nil && task == "edit" {
		applyTo = source
	}
	if strings.EqualFold(applyTo, "none") {
		applyTo = ""
	}
	rel, bytesWritten, err := e.saveVideo(generated.Video, applyTo, in.Filename)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if e.OnMutation != nil {
		e.OnMutation()
	}
	if e.OnApplied != nil {
		e.OnApplied(rel)
	}

	out := map[string]any{
		"path": rel, "bytes": bytesWritten, "mime_type": firstNonEmpty(generated.MIMEType, "video/mp4"),
		"provider": generated.Provider, "model": generated.Model, "task": task,
		"indexing": "queued", "in_place": applyTo != "",
		"note": "The generated video is in the project media bin and is being transcribed and scene-indexed. Call place_media to put it on the timeline.",
	}
	if generated.InteractionID != "" {
		out["interaction_id"] = generated.InteractionID
	}
	if applyTo != "" {
		out["applied_to"] = applyTo
	}
	if info, probeErr := ffmpeg.ProbeMedia(ctx, e.Bins, e.Workspace, rel); probeErr == nil {
		out["duration"] = info.Duration
		out["width"] = info.Width
		out["height"] = info.Height
		out["has_audio"] = info.HasAudio
	} else {
		out["probe_error"] = probeErr.Error()
	}
	return Result{OK: true, Output: out}
}

func validVideoTask(task string) bool {
	switch task {
	case "text_to_video", "image_to_video", "reference_to_video", "edit", "extend", "interpolate":
		return true
	default:
		return false
	}
}

func chooseVideoProvider(task, previous string, hasSourceVideo, hasLastFrame bool, refs, duration int, resolution string) (string, error) {
	previous = strings.TrimSpace(previous)
	resolution = strings.TrimSpace(strings.ToLower(resolution))
	veoRequired := task == "extend" || task == "interpolate" || task == "reference_to_video" || hasLastFrame || refs > 0 || duration > 0 || resolution != ""
	if previous != "" && veoRequired {
		return "", errors.New("previous_interaction_id is an Omni edit and cannot be combined with Veo-only controls")
	}
	if task == "extend" && !hasSourceVideo {
		return "", errors.New("extend requires a source video generated by Veo")
	}
	if task == "interpolate" && !hasLastFrame {
		return "", errors.New("interpolate requires last_frame and a source image")
	}
	if task == "edit" && !hasSourceVideo && previous == "" {
		return "", errors.New("edit requires a source video or previous_interaction_id")
	}
	if task == "edit" && veoRequired && previous == "" {
		return "", errors.New("edit with Veo-only controls is unsupported; use extend, interpolate, or a new Veo generation")
	}
	if veoRequired {
		return "veo", nil
	}
	return "omni", nil
}

func loadVideoInput(workspace, rel string) (gemini.VideoPart, string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return gemini.VideoPart{}, "", errors.New("media path is required")
	}
	abs, err := ffmpeg.ResolveInWorkspace(workspace, rel)
	if err != nil {
		return gemini.VideoPart{}, "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return gemini.VideoPart{}, "", err
	}
	if !info.Mode().IsRegular() {
		return gemini.VideoPart{}, "", fmt.Errorf("media path is not a file: %s", rel)
	}
	if info.Size() > maxVideoInputBytes {
		return gemini.VideoPart{}, "", fmt.Errorf("media input %s is too large (max %d bytes)", rel, maxVideoInputBytes)
	}
	ext := strings.ToLower(filepath.Ext(abs))
	kind := "video"
	mime := "video/mp4"
	if isImageExt(ext) {
		kind = "image"
		mime = imageMIME(ext)
	} else if !isVideoExt(ext) {
		return gemini.VideoPart{}, "", fmt.Errorf("unsupported video-generation input: %s", rel)
	} else {
		mime = videoMIME(ext)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return gemini.VideoPart{}, "", err
	}
	return gemini.VideoPart{Data: data, MIME: mime}, kind, nil
}

func (e VideoGenerationEnv) saveVideo(data []byte, applyTo, filename string) (string, int, error) {
	if len(data) == 0 {
		return "", 0, errors.New("generated video is empty")
	}
	if applyTo != "" {
		abs, err := ffmpeg.ResolveInWorkspace(e.Workspace, applyTo)
		if err != nil {
			return "", 0, err
		}
		if !isVideoExt(filepath.Ext(abs)) {
			return "", 0, errors.New("apply_to must be a project-relative video file")
		}
		if err := atomicWrite(abs, data); err != nil {
			return "", 0, err
		}
		rel, err := filepath.Rel(e.Workspace, abs)
		if err != nil {
			return "", 0, err
		}
		return filepath.ToSlash(rel), len(data), nil
	}

	generatedPathMu.Lock()
	defer generatedPathMu.Unlock()
	dir := filepath.Join(e.Workspace, "media")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, err
	}
	base := strings.TrimSpace(filepath.Base(filename))
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = fmt.Sprintf("video-%d", time.Now().UnixNano())
	}
	base = strings.TrimSuffix(base, filepath.Ext(base))
	if base == "" {
		base = fmt.Sprintf("video-%d", time.Now().UnixNano())
	}
	for i := 0; i < 10000; i++ {
		name := base + ".mp4"
		if i > 0 {
			name = fmt.Sprintf("%s-%d.mp4", base, i)
		}
		abs := filepath.Join(dir, name)
		if _, err := os.Stat(abs); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return "", 0, err
		}
		if err := atomicWrite(abs, data); err != nil {
			return "", 0, err
		}
		return filepath.ToSlash(filepath.Join("media", name)), len(data), nil
	}
	return "", 0, errors.New("could not allocate generated video filename")
}

func isVideoExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".mp4", ".mov", ".mkv", ".webm", ".avi", ".m4v", ".ts", ".mts":
		return true
	default:
		return false
	}
}

func imageMIME(ext string) string {
	switch strings.ToLower(ext) {
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	default:
		return "image/jpeg"
	}
}

func videoMIME(ext string) string {
	if strings.EqualFold(ext, ".webm") {
		return "video/webm"
	}
	return "video/mp4"
}
