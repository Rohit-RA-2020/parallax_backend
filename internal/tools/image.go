package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"parallax/internal/ffmpeg"
	"parallax/internal/llm"
)

const (
	DefaultGeminiBaseURL    = "https://generativelanguage.googleapis.com/v1beta"
	DefaultGeminiImageModel = "gemini-3.1-flash-image"
	defaultImageTimeout     = 3 * time.Minute
	maxImagePrompt          = 8000
	maxImageBody            = 32 << 20
	defaultImageAspect      = "16:9"
	defaultImageSize        = "1K"
	geminiImageMIME         = "image/jpeg"
	maxImageRefs            = 14
	maxImageRefBytes        = 12 << 20
)

// ImageEnv is the server-side configuration for Gemini image generation.
// API keys never enter tool arguments or tool results.
type ImageEnv struct {
	Workspace  string
	APIKey     string
	BaseURL    string
	Model      string
	Client     *http.Client
	OnMutation func()
}

func RegisterImage(reg *Registry, env ImageEnv) {
	reg.Register(llm.NewFunctionTool(
		"generate_image",
		"Generate or edit a still with Gemini and save it into the project bin under media/. For a new picture, pass only a detailed prompt. To edit an uploaded or previously generated still, pass source (or images) with the workspace path plus an edit prompt — the source bytes are sent to Gemini with the instructions. A single source is replaced in place so the bin and timeline update; pass apply_to \"none\" to keep a separate variant. Additional images can be mixed in as references. Then call place_media only when a new file should go on the timeline.",
		json.RawMessage(`{
			"type":"object",
			"properties":{
				"prompt":{"type":"string","description":"For a new still: a detailed description (subject, setting, lighting, camera, style, mood, on-image text). For an edit: what to change, keep, add, or remove. Be specific and keep identity/composition unless the user asked to restyle it."},
				"source":{"type":"string","description":"Workspace image to edit, such as media/neon-alley.jpg. Sent to Gemini with the prompt. Uploaded and generated stills are both valid."},
				"path":{"type":"string","description":"Alias for source."},
				"images":{"type":"array","items":{"type":"string"},"description":"Extra workspace image paths to use as references (style, subject, logo). Combined with source. Maximum 14 images total."},
				"apply_to":{"type":"string","description":"When editing, omit to replace the source file in place. Set to \"none\" to write a new bin item instead."},
				"aspect_ratio":{"type":"string","enum":["1:1","3:2","2:3","3:4","4:3","4:5","5:4","9:16","16:9","21:9"],"description":"Frame shape. Default 16:9 for timeline stills."},
				"image_size":{"type":"string","enum":["1K","2K","4K"],"description":"Output resolution. Default 1K; use 2K or 4K only when the user wants a high-resolution still."},
				"filename":{"type":"string","description":"Optional basename when writing a new file, such as alley-rain or title-card.jpg"}
			},
			"required":["prompt"]
		}`),
	), env.generateImage)
}

func (e ImageEnv) generateImage(ctx context.Context, raw json.RawMessage) Result {
	if strings.TrimSpace(e.APIKey) == "" {
		return Result{OK: false, Error: "Gemini is not configured; set GEMINI_API_KEY on the server"}
	}
	if strings.TrimSpace(e.Workspace) == "" {
		return Result{OK: false, Error: "workspace is not configured"}
	}

	var in struct {
		Prompt      string          `json:"prompt"`
		AspectRatio string          `json:"aspect_ratio"`
		ImageSize   string          `json:"image_size"`
		Filename    string          `json:"filename"`
		Source      string          `json:"source"`
		Path        string          `json:"path"`
		Image       string          `json:"image"`
		Images      json.RawMessage `json:"images"`
		ApplyTo     string          `json:"apply_to"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{OK: false, Error: err.Error()}
	}

	in.Prompt = strings.TrimSpace(in.Prompt)
	if in.Prompt == "" {
		return Result{OK: false, Error: "prompt is required"}
	}
	if len(in.Prompt) > maxImagePrompt {
		return Result{OK: false, Error: fmt.Sprintf("prompt must be at most %d characters", maxImagePrompt)}
	}
	aspect, ok := normalizeImageAspect(in.AspectRatio)
	if !ok {
		return Result{OK: false, Error: "aspect_ratio must be one of 1:1, 3:2, 2:3, 3:4, 4:3, 4:5, 5:4, 9:16, 16:9, 21:9"}
	}
	size, ok := normalizeImageSize(in.ImageSize)
	if !ok {
		return Result{OK: false, Error: "image_size must be 1K, 2K, or 4K"}
	}

	paths, primary, err := parseImageRefArgs(in.Source, in.Path, in.Image, in.Images)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	refs, err := e.loadImageRefs(paths)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if primary != "" && len(refs) > 0 {
		primary = refs[0].Path
	}

	generated, err := e.callGemini(ctx, in.Prompt, aspect, size, refs)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}

	applyTo, err := resolveImageApplyTo(e.Workspace, primary, in.ApplyTo)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}

	var rel string
	var bytesWritten int
	if applyTo != "" {
		rel, bytesWritten, err = replaceGeneratedImage(e.Workspace, applyTo, generated)
	} else {
		filename := in.Filename
		if filename == "" && primary != "" {
			filename = editFilename(primary)
		}
		rel, bytesWritten, err = writeGeneratedImage(e.Workspace, filename, in.Prompt, generated)
	}
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if e.OnMutation != nil {
		e.OnMutation()
	}

	width, height := imageDimensions(generated.Bytes)
	refPaths := make([]string, 0, len(refs))
	for _, ref := range refs {
		refPaths = append(refPaths, ref.Path)
	}
	out := map[string]any{
		"path":         rel,
		"bytes":        bytesWritten,
		"mime_type":    generated.MIME,
		"aspect_ratio": aspect,
		"image_size":   size,
		"model":        e.modelName(),
		"edited":       len(refs) > 0,
	}
	if len(refPaths) > 0 {
		out["references"] = refPaths
	}
	if primary != "" {
		out["source"] = primary
	}
	if applyTo != "" {
		out["applied_to"] = applyTo
		out["in_place"] = true
		out["note"] = "The still was edited in place. The project still has one current version of this file; timeline clips that use it will show the new picture."
	} else if len(refs) > 0 {
		out["in_place"] = false
		out["note"] = "The edited still is a new file in the project bin. Call place_media with this path to put it on the timeline, or update the existing clip to this path."
	} else {
		out["in_place"] = false
		out["note"] = "The still is in the project bin. Call place_media with this path to put it on the timeline."
	}
	if width > 0 && height > 0 {
		out["width"] = width
		out["height"] = height
	}
	if generated.Text != "" {
		out["model_text"] = trimOutput(generated.Text, 2000)
	}
	return Result{OK: true, Output: out}
}

type geminiImage struct {
	Bytes []byte
	MIME  string
	Text  string
}

type imageRef struct {
	Path string
	MIME string
	Data []byte
}

func parseImageRefArgs(source, path, image string, images json.RawMessage) ([]string, string, error) {
	primary := firstFilled(source, path, image)
	extras, err := parseStringList(images)
	if err != nil {
		return nil, "", err
	}
	if primary == "" && len(extras) == 1 {
		primary = extras[0]
		extras = nil
	}
	var paths []string
	seen := map[string]bool{}
	add := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" || seen[raw] {
			return
		}
		seen[raw] = true
		paths = append(paths, raw)
	}
	add(primary)
	for _, extra := range extras {
		add(extra)
	}
	if len(paths) > maxImageRefs {
		return nil, "", fmt.Errorf("at most %d reference images are allowed", maxImageRefs)
	}
	return paths, strings.TrimSpace(primary), nil
}

func parseStringList(raw json.RawMessage) ([]string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		one = strings.TrimSpace(one)
		if one == "" {
			return nil, nil
		}
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, fmt.Errorf("images must be a path or an array of paths")
	}
	out := make([]string, 0, len(many))
	for _, item := range many {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out, nil
}

func firstFilled(vals ...string) string {
	for _, val := range vals {
		if val = strings.TrimSpace(val); val != "" {
			return val
		}
	}
	return ""
}

func (e ImageEnv) loadImageRefs(paths []string) ([]imageRef, error) {
	refs := make([]imageRef, 0, len(paths))
	for _, raw := range paths {
		ref, err := loadWorkspaceImage(e.Workspace, raw)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

func loadWorkspaceImage(workspace, rel string) (imageRef, error) {
	abs, err := ffmpeg.ResolveInWorkspace(workspace, rel)
	if err != nil {
		return imageRef{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return imageRef{}, fmt.Errorf("image not found: %s", rel)
		}
		return imageRef{}, err
	}
	if !info.Mode().IsRegular() {
		return imageRef{}, fmt.Errorf("image path is not a file: %s", rel)
	}
	if !isImageExt(filepath.Ext(abs)) {
		return imageRef{}, fmt.Errorf("source must be an image file: %s", rel)
	}
	if info.Size() > maxImageRefBytes {
		return imageRef{}, fmt.Errorf("image %s is too large (max %d bytes)", rel, maxImageRefBytes)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return imageRef{}, err
	}
	if !looksLikeImage(data) {
		return imageRef{}, fmt.Errorf("file is not a readable image: %s", rel)
	}
	clean, err := filepath.Rel(workspace, abs)
	if err != nil {
		return imageRef{}, err
	}
	return imageRef{
		Path: filepath.ToSlash(clean),
		MIME: sniffImageMIME(data),
		Data: data,
	}, nil
}

func resolveImageApplyTo(workspace, source, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if strings.EqualFold(raw, "none") || strings.EqualFold(raw, "false") || raw == "-" {
		return "", nil
	}
	if raw != "" {
		abs, err := ffmpeg.ResolveInWorkspace(workspace, raw)
		if err != nil {
			return "", fmt.Errorf("apply_to: %w", err)
		}
		rel, err := filepath.Rel(workspace, abs)
		if err != nil {
			return "", err
		}
		return filepath.ToSlash(rel), nil
	}
	return strings.TrimSpace(source), nil
}

func replaceGeneratedImage(workspace, destRel string, img geminiImage) (string, int, error) {
	abs, err := ffmpeg.ResolveInWorkspace(workspace, destRel)
	if err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".generated-*")
	if err != nil {
		return "", 0, err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(img.Bytes); err != nil {
		return "", 0, err
	}
	if err := tmp.Sync(); err != nil {
		return "", 0, err
	}
	if err := tmp.Close(); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmpName, abs); err != nil {
		return "", 0, err
	}
	ok = true
	rel, err := filepath.Rel(workspace, abs)
	if err != nil {
		return "", 0, err
	}
	return filepath.ToSlash(rel), len(img.Bytes), nil
}

func editFilename(source string) string {
	base := filepath.Base(source)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if stem == "" {
		return "edited"
	}
	return stem + "-edit"
}

func isImageExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".bmp", ".tif", ".tiff":
		return true
	}
	return false
}

func (e ImageEnv) callGemini(ctx context.Context, prompt, aspect, size string, refs []imageRef) (geminiImage, error) {
	endpoint, err := geminiInteractionsURL(e.BaseURL)
	if err != nil {
		return geminiImage{}, err
	}
	input := make([]map[string]string, 0, 1+len(refs))
	input = append(input, map[string]string{"type": "text", "text": prompt})
	for _, ref := range refs {
		input = append(input, map[string]string{
			"type":      "image",
			"mime_type": ref.MIME,
			"data":      base64.StdEncoding.EncodeToString(ref.Data),
		})
	}
	payload, err := json.Marshal(map[string]any{
		"model": e.modelName(),
		"input": input,
		"response_format": map[string]string{
			"type":         "image",
			"mime_type":    geminiImageMIME,
			"aspect_ratio": aspect,
			"image_size":   size,
		},
		"store": false,
	})
	if err != nil {
		return geminiImage{}, fmt.Errorf("failed to encode Gemini request: %w", err)
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, defaultImageTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(timeoutCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return geminiImage{}, fmt.Errorf("failed to create Gemini request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", strings.TrimSpace(e.APIKey))

	client := e.Client
	if client == nil {
		client = &http.Client{}
	}
	res, err := client.Do(req)
	if err != nil {
		if timeoutCtx.Err() != nil {
			return geminiImage{}, fmt.Errorf("Gemini image generation timed out or was canceled: %w", timeoutCtx.Err())
		}
		return geminiImage{}, fmt.Errorf("Gemini request failed: %w", err)
	}
	defer res.Body.Close()

	data, err := io.ReadAll(io.LimitReader(res.Body, maxImageBody+1))
	if err != nil {
		return geminiImage{}, fmt.Errorf("failed to read Gemini response: %w", err)
	}
	if len(data) > maxImageBody {
		return geminiImage{}, fmt.Errorf("Gemini response was too large")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return geminiImage{}, fmt.Errorf("Gemini returned HTTP %s: %s", res.Status, compactErrorBody(data))
	}

	img, err := extractGeneratedImage(data)
	if err != nil {
		return geminiImage{}, err
	}
	return img, nil
}

func (e ImageEnv) modelName() string {
	if model := strings.TrimSpace(e.Model); model != "" {
		return model
	}
	return DefaultGeminiImageModel
}

func geminiInteractionsURL(base string) (string, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = DefaultGeminiBaseURL
	}
	if !strings.HasSuffix(base, "/interactions") {
		base += "/interactions"
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid Gemini base URL %q", base)
	}
	return u.String(), nil
}

func extractGeneratedImage(raw []byte) (geminiImage, error) {
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return geminiImage{}, fmt.Errorf("invalid JSON from Gemini: %w", err)
	}
	img := pickGeneratedImage(payload)
	if len(img.Bytes) == 0 {
		if img.Text != "" {
			return geminiImage{}, fmt.Errorf("Gemini did not return an image: %s", trimOutput(img.Text, 800))
		}
		return geminiImage{}, fmt.Errorf("Gemini did not return an image")
	}
	if img.MIME == "" {
		img.MIME = sniffImageMIME(img.Bytes)
	}
	return img, nil
}

func pickGeneratedImage(node any) geminiImage {
	var img geminiImage
	if obj, ok := node.(map[string]any); ok {
		if out, exists := obj["output_image"]; exists {
			collectGeneratedImage(out, &img, "image")
		}
		if len(img.Bytes) == 0 {
			if steps, exists := obj["steps"]; exists {
				collectModelOutputImages(steps, &img)
			}
		}
		if img.Text == "" {
			img.Text = firstString(obj, "output_text")
		}
	}
	if len(img.Bytes) == 0 {
		collectGeneratedImage(node, &img, "")
	}
	return img
}

func collectModelOutputImages(node any, img *geminiImage) {
	steps, ok := node.([]any)
	if !ok {
		return
	}
	for _, step := range steps {
		obj, ok := step.(map[string]any)
		if !ok {
			continue
		}
		kind := strings.ToLower(firstString(obj, "type"))
		if kind != "" && kind != "model_output" {
			if text := firstString(obj, "text"); text != "" && img.Text == "" {
				img.Text = text
			}
			continue
		}
		if content, exists := obj["content"]; exists {
			collectGeneratedImage(content, img, "image")
		}
		if len(img.Bytes) > 0 {
			return
		}
	}
}

func collectGeneratedImage(node any, img *geminiImage, hint string) {
	if img == nil || len(img.Bytes) > 0 {
		return
	}
	switch v := node.(type) {
	case map[string]any:
		if mime := firstString(v, "mime_type", "mimeType"); mime != "" && strings.HasPrefix(strings.ToLower(mime), "image/") {
			if data := firstString(v, "data", "b64_json", "b64"); data != "" {
				if decoded, err := decodeImageBase64(data); err == nil && looksLikeImage(decoded) {
					img.Bytes = decoded
					img.MIME = mime
					return
				}
			}
		}
		if data := firstString(v, "data", "b64_json", "b64"); data != "" && looksLikeImageKey(hint, v) {
			if decoded, err := decodeImageBase64(data); err == nil && looksLikeImage(decoded) {
				img.Bytes = decoded
				img.MIME = firstString(v, "mime_type", "mimeType")
				if img.MIME == "" {
					img.MIME = sniffImageMIME(decoded)
				}
				return
			}
		}
		if inline, ok := v["inlineData"]; ok {
			collectGeneratedImage(inline, img, "image")
		}
		if inline, ok := v["inline_data"]; ok {
			collectGeneratedImage(inline, img, "image")
		}
		if out, ok := v["output_image"]; ok {
			collectGeneratedImage(out, img, "image")
		}
		if text := firstString(v, "text", "output_text"); text != "" && img.Text == "" {
			img.Text = text
		}
		for key, child := range v {
			if len(img.Bytes) > 0 {
				return
			}
			nextHint := hint
			if looksLikeImageKey(key, nil) {
				nextHint = "image"
			}
			collectGeneratedImage(child, img, nextHint)
		}
	case []any:
		for _, child := range v {
			collectGeneratedImage(child, img, hint)
			if len(img.Bytes) > 0 {
				return
			}
		}
	}
}

func looksLikeImageKey(key string, obj map[string]any) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	switch key {
	case "image", "output_image", "inlinedata", "inline_data":
		return true
	}
	if obj == nil {
		return false
	}
	kind := strings.ToLower(firstString(obj, "type"))
	return kind == "image" || strings.HasPrefix(strings.ToLower(firstString(obj, "mime_type", "mimeType")), "image/")
}

func firstString(obj map[string]any, keys ...string) string {
	for _, key := range keys {
		if raw, ok := obj[key]; ok {
			if s, ok := raw.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

func decodeImageBase64(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if i := strings.Index(value, ","); i >= 0 && strings.Contains(strings.ToLower(value[:i]), "base64") {
		value = value[i+1:]
	}
	value = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, value)
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(value)
	}
	return decoded, err
}

func writeGeneratedImage(workspace, filename, prompt string, img geminiImage) (string, int, error) {
	ext := extensionForMIME(img.MIME)
	name := sanitizeImageName(filename)
	if name == "" {
		name = slugFromPrompt(prompt)
	}
	if name == "" {
		name = "generated"
	}
	if filepath.Ext(name) == "" || !sameImageExt(filepath.Ext(name), ext) {
		name = strings.TrimSuffix(name, filepath.Ext(name)) + ext
	}

	dir := filepath.Join(workspace, "media")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, err
	}
	dst := availableMediaPath(dir, name)
	tmp, err := os.CreateTemp(dir, ".generated-*")
	if err != nil {
		return "", 0, err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(img.Bytes); err != nil {
		return "", 0, err
	}
	if err := tmp.Sync(); err != nil {
		return "", 0, err
	}
	if err := tmp.Close(); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return "", 0, err
	}
	ok = true
	rel, err := filepath.Rel(workspace, dst)
	if err != nil {
		return "", 0, err
	}
	return filepath.ToSlash(rel), len(img.Bytes), nil
}

func availableMediaPath(dir, name string) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 0; ; i++ {
		candidate := filepath.Join(dir, name)
		if i > 0 {
			candidate = filepath.Join(dir, fmt.Sprintf("%s-%d%s", base, i, ext))
		}
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

func sanitizeImageName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, " ", "-")
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	cleaned := strings.Trim(b.String(), ".-_")
	for strings.Contains(cleaned, "--") {
		cleaned = strings.ReplaceAll(cleaned, "--", "-")
	}
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return ""
	}
	return cleaned
}

func slugFromPrompt(prompt string) string {
	var words []string
	var current strings.Builder
	flush := func() {
		if current.Len() == 0 {
			return
		}
		words = append(words, strings.ToLower(current.String()))
		current.Reset()
	}
	for _, r := range prompt {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current.WriteRune(unicode.ToLower(r))
			continue
		}
		flush()
		if len(words) >= 6 {
			break
		}
	}
	flush()
	if len(words) == 0 {
		return ""
	}
	slug := strings.Join(words, "-")
	if len(slug) > 48 {
		slug = strings.Trim(slug[:48], "-")
	}
	return slug
}

func normalizeImageAspect(value string) (string, bool) {
	switch value = strings.TrimSpace(value); value {
	case "":
		return defaultImageAspect, true
	case "1:1", "3:2", "2:3", "3:4", "4:3", "4:5", "5:4", "9:16", "16:9", "21:9":
		return value, true
	default:
		return "", false
	}
}

func normalizeImageSize(value string) (string, bool) {
	switch value = strings.TrimSpace(value); value {
	case "":
		return defaultImageSize, true
	case "1K", "2K", "4K":
		return value, true
	case "1k":
		return "1K", true
	case "2k":
		return "2K", true
	case "4k":
		return "4K", true
	default:
		return "", false
	}
}

func extensionForMIME(mime string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".png"
	}
}

func sameImageExt(got, want string) bool {
	got = strings.ToLower(got)
	want = strings.ToLower(want)
	if got == ".jpeg" {
		got = ".jpg"
	}
	if want == ".jpeg" {
		want = ".jpg"
	}
	return got != "" && got == want
}

func sniffImageMIME(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("RIFF")) && len(data) >= 12 && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp"
	case bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	default:
		return "image/png"
	}
}

func looksLikeImage(data []byte) bool {
	if len(data) < 8 {
		return false
	}
	mime := sniffImageMIME(data)
	return mime != "" && (bytes.HasPrefix(data, []byte("\x89PNG")) || bytes.HasPrefix(data, []byte("\xff\xd8\xff")) || bytes.HasPrefix(data, []byte("RIFF")) || bytes.HasPrefix(data, []byte("GIF8")))
}

func imageDimensions(data []byte) (int, int) {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		if len(data) < 24 {
			return 0, 0
		}
		return int(binary.BigEndian.Uint32(data[16:20])), int(binary.BigEndian.Uint32(data[20:24]))
	case bytes.HasPrefix(data, []byte("\xff\xd8\xff")):
		return jpegDimensions(data)
	default:
		return 0, 0
	}
}

func jpegDimensions(data []byte) (int, int) {
	for i := 2; i < len(data)-8; {
		if data[i] != 0xff {
			return 0, 0
		}
		marker := data[i+1]
		if marker == 0xd8 || marker == 0xd9 {
			i += 2
			continue
		}
		if i+3 >= len(data) {
			return 0, 0
		}
		size := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if size < 2 || i+2+size > len(data) {
			return 0, 0
		}
		if marker >= 0xc0 && marker <= 0xc3 {
			if i+8 >= len(data) {
				return 0, 0
			}
			return int(binary.BigEndian.Uint16(data[i+7 : i+9])), int(binary.BigEndian.Uint16(data[i+5 : i+7]))
		}
		i += 2 + size
	}
	return 0, 0
}
