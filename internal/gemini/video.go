package gemini

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

const (
	DefaultOmniVideoModel = "gemini-omni-flash-preview"
	DefaultVeoVideoModel  = "veo-3.1-generate-preview"
	DefaultVideoPoll      = 5 * time.Second
)

// VideoPart is a local image or video supplied to Gemini.
type VideoPart struct {
	Data []byte
	MIME string
}

// VideoRequest is the provider-neutral request used by Director's video tool.
type VideoRequest struct {
	Model                 string
	Prompt                string
	Task                  string
	SourceImage           *VideoPart
	SourceVideo           *VideoPart
	ReferenceImages       []VideoPart
	LastFrame             *VideoPart
	PreviousInteractionID string
	AspectRatio           string
	DurationSeconds       int
	Resolution            string
	PollInterval          time.Duration
}

type VideoResult struct {
	Provider      string
	Model         string
	InteractionID string
	VideoURI      string
	Video         []byte
	MIMEType      string
}

// GenerateOmni uses the Gemini Interactions API. It accepts inline parts for
// local project media and downloads URI-delivered output before returning.
func (c *Client) GenerateOmni(ctx context.Context, in VideoRequest) (VideoResult, error) {
	if c == nil || strings.TrimSpace(c.APIKey) == "" {
		return VideoResult{}, errors.New("Gemini video: API key is not configured")
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return VideoResult{}, errors.New("Gemini video: prompt is required")
	}
	if strings.TrimSpace(in.Model) == "" {
		in.Model = DefaultOmniVideoModel
	}
	if in.AspectRatio == "" {
		in.AspectRatio = "16:9"
	}
	if in.AspectRatio != "16:9" && in.AspectRatio != "9:16" {
		return VideoResult{}, errors.New("Gemini Omni: aspect_ratio must be 16:9 or 9:16")
	}

	var input any = strings.TrimSpace(in.Prompt)
	parts := make([]map[string]string, 0, 2+len(in.ReferenceImages))
	if in.SourceImage != nil {
		parts = append(parts, inlinePart("image", *in.SourceImage))
	}
	if in.SourceVideo != nil {
		parts = append(parts, inlinePart("video", *in.SourceVideo))
	}
	for _, ref := range in.ReferenceImages {
		parts = append(parts, inlinePart("image", ref))
	}
	if len(parts) > 0 {
		parts = append(parts, map[string]string{"type": "text", "text": strings.TrimSpace(in.Prompt)})
		input = parts
	}

	payload := map[string]any{
		"model": in.Model,
		"input": input,
		"response_format": map[string]any{
			"type":         "video",
			"delivery":     "uri",
			"aspect_ratio": in.AspectRatio,
		},
	}
	if in.PreviousInteractionID != "" {
		payload["previous_interaction_id"] = strings.TrimSpace(in.PreviousInteractionID)
	}
	if in.Task != "" {
		payload["generation_config"] = map[string]any{"video_config": map[string]string{"task": in.Task}}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return VideoResult{}, fmt.Errorf("Gemini Omni: encode request: %w", err)
	}
	endpoint, err := interactionsURL(c.BaseURL)
	if err != nil {
		return VideoResult{}, err
	}
	raw, err := c.doJSON(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return VideoResult{}, fmt.Errorf("Gemini Omni: %w", err)
	}
	result, err := parseOmniVideo(raw)
	if err != nil {
		return VideoResult{}, err
	}
	result.Provider = "omni"
	result.Model = in.Model
	if len(result.Video) == 0 && result.VideoURI != "" {
		result.Video, err = c.downloadVideoURI(ctx, result.VideoURI, in.PollInterval)
		if err != nil {
			return VideoResult{}, err
		}
	}
	if len(result.Video) == 0 {
		return VideoResult{}, errors.New("Gemini Omni: response did not contain a video")
	}
	if result.MIMEType == "" {
		result.MIMEType = "video/mp4"
	}
	return result, nil
}

// GenerateVeo submits and polls a Veo long-running operation.
func (c *Client) GenerateVeo(ctx context.Context, in VideoRequest) (VideoResult, error) {
	if c == nil || strings.TrimSpace(c.APIKey) == "" {
		return VideoResult{}, errors.New("Gemini video: API key is not configured")
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return VideoResult{}, errors.New("Gemini Veo: prompt is required")
	}
	if strings.TrimSpace(in.Model) == "" {
		in.Model = DefaultVeoVideoModel
	}
	if in.AspectRatio == "" {
		in.AspectRatio = "16:9"
	}
	if in.AspectRatio != "16:9" && in.AspectRatio != "9:16" {
		return VideoResult{}, errors.New("Gemini Veo: aspect_ratio must be 16:9 or 9:16")
	}
	if in.DurationSeconds == 0 {
		in.DurationSeconds = 8
	}
	if in.DurationSeconds != 4 && in.DurationSeconds != 6 && in.DurationSeconds != 8 {
		return VideoResult{}, errors.New("Gemini Veo: duration_seconds must be 4, 6, or 8")
	}
	if in.Resolution == "" {
		in.Resolution = "720p"
	}
	if in.Resolution != "720p" && in.Resolution != "1080p" && in.Resolution != "4k" {
		return VideoResult{}, errors.New("Gemini Veo: resolution must be 720p, 1080p, or 4k")
	}
	if (in.Resolution == "1080p" || in.Resolution == "4k") && in.DurationSeconds != 8 {
		return VideoResult{}, errors.New("Gemini Veo: 1080p and 4k require duration_seconds 8")
	}
	if (in.LastFrame != nil || len(in.ReferenceImages) > 0 || in.SourceVideo != nil) && in.DurationSeconds != 8 {
		return VideoResult{}, errors.New("Gemini Veo: interpolation, references, and extension require duration_seconds 8")
	}
	if in.Resolution != "720p" && in.SourceVideo != nil {
		return VideoResult{}, errors.New("Gemini Veo: video extension is limited to 720p")
	}
	if len(in.ReferenceImages) > 3 {
		return VideoResult{}, errors.New("Gemini Veo: at most 3 reference images are supported")
	}

	instance := map[string]any{"prompt": strings.TrimSpace(in.Prompt)}
	if in.SourceImage != nil {
		instance["image"] = inlineObject(*in.SourceImage)
	}
	if in.LastFrame != nil {
		if in.SourceImage == nil {
			return VideoResult{}, errors.New("Gemini Veo: last_frame requires a source image")
		}
		instance["lastFrame"] = inlineObject(*in.LastFrame)
	}
	if in.SourceVideo != nil {
		instance["video"] = inlineObject(*in.SourceVideo)
	}
	if len(in.ReferenceImages) > 0 {
		refs := make([]map[string]any, 0, len(in.ReferenceImages))
		for _, ref := range in.ReferenceImages {
			refs = append(refs, map[string]any{"image": inlineObject(ref), "referenceType": "asset"})
		}
		instance["referenceImages"] = refs
	}
	payload := map[string]any{
		"instances": []any{instance},
		"parameters": map[string]any{
			"aspectRatio":     in.AspectRatio,
			"durationSeconds": fmt.Sprintf("%d", in.DurationSeconds),
			"resolution":      in.Resolution,
			"numberOfVideos":  1,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return VideoResult{}, fmt.Errorf("Gemini Veo: encode request: %w", err)
	}
	endpoint, err := veoGenerateURL(c.BaseURL, in.Model)
	if err != nil {
		return VideoResult{}, err
	}
	raw, err := c.doJSON(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return VideoResult{}, fmt.Errorf("Gemini Veo: %w", err)
	}
	var start struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &start); err != nil || strings.TrimSpace(start.Name) == "" {
		return VideoResult{}, fmt.Errorf("Gemini Veo: response did not contain an operation name")
	}
	operation, err := c.pollVeo(ctx, start.Name, in.PollInterval)
	if err != nil {
		return VideoResult{}, err
	}
	result, err := parseVeoVideo(operation)
	if err != nil {
		return VideoResult{}, err
	}
	result.Provider = "veo"
	result.Model = in.Model
	if len(result.Video) == 0 && result.VideoURI != "" {
		result.Video, err = c.downloadVideoURI(ctx, result.VideoURI, in.PollInterval)
		if err != nil {
			return VideoResult{}, err
		}
	}
	if len(result.Video) == 0 {
		return VideoResult{}, errors.New("Gemini Veo: response did not contain a video")
	}
	if result.MIMEType == "" {
		result.MIMEType = "video/mp4"
	}
	return result, nil
}

func inlinePart(kind string, part VideoPart) map[string]string {
	return map[string]string{
		"type":      kind,
		"mime_type": firstNonEmpty(part.MIME, "video/mp4"),
		"data":      base64.StdEncoding.EncodeToString(part.Data),
	}
}

func inlineObject(part VideoPart) map[string]any {
	return map[string]any{"inlineData": map[string]string{
		"mimeType": firstNonEmpty(part.MIME, "video/mp4"),
		"data":     base64.StdEncoding.EncodeToString(part.Data),
	}}
}

func (c *Client) doJSON(ctx context.Context, method, endpoint string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", strings.TrimSpace(c.APIKey))
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponseBytes()+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if int64(len(raw)) > c.maxResponseBytes() {
		return nil, fmt.Errorf("response exceeds %d bytes", c.maxResponseBytes())
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &ProviderError{StatusCode: resp.StatusCode, Body: string(raw)}
	}
	return raw, nil
}

func (c *Client) pollVeo(ctx context.Context, operation string, interval time.Duration) ([]byte, error) {
	if interval <= 0 {
		interval = DefaultVideoPoll
	}
	endpoint, err := resolveOperationURL(c.BaseURL, operation)
	if err != nil {
		return nil, err
	}
	for {
		raw, err := c.doJSON(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("poll operation: %w", err)
		}
		var state struct {
			Done  bool            `json:"done"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(raw, &state); err != nil {
			return nil, fmt.Errorf("decode operation: %w", err)
		}
		if state.Done {
			if len(state.Error) > 0 && string(state.Error) != "null" {
				return nil, fmt.Errorf("Gemini Veo: operation failed: %s", trimJSON(state.Error))
			}
			return raw, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) downloadVideoURI(ctx context.Context, rawURI string, interval time.Duration) ([]byte, error) {
	u, err := url.Parse(strings.TrimSpace(rawURI))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("Gemini video: invalid video URI")
	}
	fileID := fileIDFromURI(u.Path)
	if fileID != "" {
		statusURL := strings.TrimRight(c.BaseURL, "/") + "/files/" + url.PathEscape(fileID)
		for {
			body, statusErr := c.doJSON(ctx, http.MethodGet, statusURL, nil)
			if statusErr == nil {
				var state struct {
					State string `json:"state"`
				}
				if json.Unmarshal(body, &state) == nil {
					switch strings.ToUpper(state.State) {
					case "FAILED", "ERROR":
						return nil, fmt.Errorf("Gemini video file processing failed")
					case "ACTIVE":
						goto download
					}
				}
			}
			if interval <= 0 {
				interval = DefaultVideoPoll
			}
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	}

download:
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-goog-api-key", strings.TrimSpace(c.APIKey))
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("download video: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &ProviderError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponseBytes()+1))
	if err != nil {
		return nil, fmt.Errorf("read video: %w", err)
	}
	if int64(len(data)) > c.maxResponseBytes() {
		return nil, fmt.Errorf("video exceeds %d bytes", c.maxResponseBytes())
	}
	return data, nil
}

func parseOmniVideo(raw []byte) (VideoResult, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return VideoResult{}, fmt.Errorf("Gemini Omni: decode response: %w", err)
	}
	result := VideoResult{InteractionID: firstString(root, "id", "interaction_id")}
	collectVideoValue(root["output_video"], &result)
	if steps, ok := root["steps"].([]any); ok {
		for _, step := range steps {
			if obj, ok := step.(map[string]any); ok {
				collectVideoValue(obj["content"], &result)
			}
		}
	}
	return result, nil
}

func collectVideoValue(value any, result *VideoResult) {
	switch item := value.(type) {
	case map[string]any:
		kind := strings.ToLower(firstString(item, "type"))
		mime := firstString(item, "mime_type", "mimeType")
		if kind == "video" || strings.HasPrefix(strings.ToLower(mime), "video/") {
			if encoded := firstString(item, "data", "b64_json", "b64"); encoded != "" {
				if data, err := base64.StdEncoding.DecodeString(encoded); err == nil {
					result.Video = data
					result.MIMEType = mime
				}
			}
			if uri := firstString(item, "uri", "url"); uri != "" {
				result.VideoURI = uri
				result.MIMEType = mime
			}
		}
		for _, child := range item {
			collectVideoValue(child, result)
		}
	case []any:
		for _, child := range item {
			collectVideoValue(child, result)
		}
	}
}

func parseVeoVideo(raw []byte) (VideoResult, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return VideoResult{}, fmt.Errorf("Gemini Veo: decode operation: %w", err)
	}
	result := VideoResult{}
	if response, ok := root["response"]; ok {
		collectVeoValue(response, &result)
	}
	if result.VideoURI == "" {
		collectVeoValue(root, &result)
	}
	return result, nil
}

func collectVeoValue(value any, result *VideoResult) {
	switch item := value.(type) {
	case map[string]any:
		if uri := firstString(item, "uri", "url"); uri != "" {
			result.VideoURI = uri
		}
		if encoded := firstString(item, "bytesBase64Encoded", "data", "videoBytes"); encoded != "" {
			if data, err := base64.StdEncoding.DecodeString(encoded); err == nil {
				result.Video = data
				result.MIMEType = firstNonEmpty(firstString(item, "mimeType", "mime_type"), "video/mp4")
			}
		}
		for _, child := range item {
			collectVeoValue(child, result)
		}
	case []any:
		for _, child := range item {
			collectVeoValue(child, result)
		}
	}
}

func veoGenerateURL(base, model string) (string, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(base), "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("Gemini Veo: invalid base URL %q", base)
	}
	u.Path = path.Join(u.Path, "models", model+":predictLongRunning")
	return u.String(), nil
}

func resolveOperationURL(base, operation string) (string, error) {
	if u, err := url.Parse(operation); err == nil && u.Scheme != "" && u.Host != "" {
		return u.String(), nil
	}
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(base), "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("Gemini Veo: invalid operation URL")
	}
	u.Path = path.Join(u.Path, strings.TrimPrefix(operation, "/"))
	return u.String(), nil
}

var fileIDPattern = regexp.MustCompile(`/files/([^/:]+)`)

func fileIDFromURI(rawPath string) string {
	m := fileIDPattern.FindStringSubmatch(rawPath)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

func trimJSON(raw []byte) string {
	if len(raw) > 2000 {
		return string(raw[:2000]) + "…"
	}
	return string(raw)
}

func (c *Client) httpClient() *http.Client {
	if c != nil && c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
