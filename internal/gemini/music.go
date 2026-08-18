// Package gemini contains the small REST clients used by Director's Gemini
// generation tools. It intentionally uses net/http rather than a Python
// process or provider SDK.
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
	"strings"
	"time"
)

const (
	DefaultBaseURL         = "https://generativelanguage.googleapis.com/v1beta"
	DefaultMusicModel      = "lyria-3-pro-preview"
	DefaultMusicOutput     = "mp3"
	DefaultMaxResponseSize = 256 << 20
)

// Client calls Gemini's Interactions API for Lyria music generation.
type Client struct {
	APIKey           string
	BaseURL          string
	HTTPClient       *http.Client
	MaxResponseBytes int64
}

type MusicRequest struct {
	Model        string
	Prompt       string
	OutputFormat string
}

type MusicResult struct {
	InteractionID string
	Audio         []byte
	Lyrics        string
	MIMEType      string
}

type ProviderError struct {
	StatusCode int
	Body       string
}

func (e *ProviderError) Error() string {
	if e == nil {
		return "Gemini provider error"
	}
	body := strings.TrimSpace(e.Body)
	if len(body) > 2000 {
		body = body[:2000] + "…"
	}
	if body == "" {
		body = http.StatusText(e.StatusCode)
	}
	return fmt.Sprintf("Gemini music: http %d: %s", e.StatusCode, body)
}

func NewClient(apiKey, baseURL string, timeout time.Duration, maxResponseBytes int64) *Client {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURL
	}
	if timeout < time.Second {
		timeout = 15 * time.Minute
	}
	if maxResponseBytes < 1<<20 {
		maxResponseBytes = DefaultMaxResponseSize
	}
	return &Client{
		APIKey:           strings.TrimSpace(apiKey),
		BaseURL:          strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		MaxResponseBytes: maxResponseBytes,
		HTTPClient: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          16,
				MaxIdleConnsPerHost:   8,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   15 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
	}
}

func (c *Client) GenerateMusic(ctx context.Context, in MusicRequest) (MusicResult, error) {
	if strings.TrimSpace(in.Prompt) == "" {
		return MusicResult{}, errors.New("Gemini music: prompt is required")
	}
	if strings.TrimSpace(in.Model) == "" {
		in.Model = DefaultMusicModel
	}
	format, err := normalizeOutputFormat(in.OutputFormat)
	if err != nil {
		return MusicResult{}, err
	}
	payload := map[string]any{
		"model": in.Model,
		"input": strings.TrimSpace(in.Prompt),
	}
	// Lyria 3 returns MP3 by default. The Interactions API documents the
	// audio response format for Pro when WAV is requested.
	if format == "wav" {
		if strings.TrimSpace(in.Model) != "lyria-3-pro-preview" {
			return MusicResult{}, errors.New("Gemini music: WAV output is only supported with lyria-3-pro-preview")
		}
		payload["response_format"] = map[string]string{"type": "audio"}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return MusicResult{}, fmt.Errorf("Gemini music: encode request: %w", err)
	}
	endpoint, err := interactionsURL(c.BaseURL)
	if err != nil {
		return MusicResult{}, err
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return MusicResult{}, errors.New("Gemini music: API key is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return MusicResult{}, fmt.Errorf("Gemini music: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", strings.TrimSpace(c.APIKey))
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return MusicResult{}, fmt.Errorf("Gemini music request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponseBytes()+1))
	if err != nil {
		return MusicResult{}, fmt.Errorf("Gemini music: read response: %w", err)
	}
	if int64(len(raw)) > c.maxResponseBytes() {
		return MusicResult{}, fmt.Errorf("Gemini music: response exceeds %d bytes", c.maxResponseBytes())
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return MusicResult{}, &ProviderError{StatusCode: resp.StatusCode, Body: string(raw)}
	}
	result, err := parseMusicResponse(raw)
	if err != nil {
		return MusicResult{}, err
	}
	if len(result.Audio) == 0 {
		return MusicResult{}, errors.New("Gemini music: response did not contain audio")
	}
	if result.MIMEType == "" {
		if format == "wav" {
			result.MIMEType = "audio/wav"
		} else {
			result.MIMEType = "audio/mpeg"
		}
	}
	return result, nil
}

func interactionsURL(base string) (string, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	if !strings.HasSuffix(base, "/interactions") {
		base += "/interactions"
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("Gemini music: invalid base URL %q", base)
	}
	return u.String(), nil
}

func normalizeOutputFormat(format string) (string, error) {
	format = strings.ToLower(strings.TrimSpace(format))
	format = strings.TrimPrefix(format, "audio/")
	format = strings.TrimPrefix(format, ".")
	if format == "" || format == "auto" || format == "mp3" || strings.HasPrefix(format, "mp3_") {
		return "mp3", nil
	}
	if format == "wav" || format == "audio/wav" {
		return "wav", nil
	}
	return "", fmt.Errorf("Gemini music: output_format must be mp3 or wav")
}

func (c *Client) maxResponseBytes() int64 {
	if c != nil && c.MaxResponseBytes > 0 {
		return c.MaxResponseBytes
	}
	return DefaultMaxResponseSize
}

func parseMusicResponse(raw []byte) (MusicResult, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return MusicResult{}, fmt.Errorf("Gemini music: decode response: %w", err)
	}
	result := MusicResult{
		InteractionID: firstString(root, "id", "interaction_id"),
		Lyrics:        firstString(root, "output_text"),
	}
	if output, ok := root["output_audio"]; ok {
		collectAudio(output, &result)
	}
	if steps, ok := root["steps"]; ok {
		collectSteps(steps, &result, result.Lyrics == "")
	}
	if result.Lyrics == "" {
		result.Lyrics = strings.TrimSpace(result.Lyrics)
	}
	return result, nil
}

func collectSteps(value any, result *MusicResult, includeText bool) {
	steps, ok := value.([]any)
	if !ok {
		return
	}
	for _, step := range steps {
		obj, ok := step.(map[string]any)
		if !ok {
			continue
		}
		if content, ok := obj["content"]; ok {
			collectContent(content, result, includeText)
		}
	}
}

func collectContent(value any, result *MusicResult, includeText bool) {
	content, ok := value.([]any)
	if !ok {
		return
	}
	for _, item := range content {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if includeText && strings.TrimSpace(firstString(obj, "type")) == "text" {
			text := strings.TrimSpace(firstString(obj, "text"))
			if text != "" {
				if result.Lyrics != "" {
					result.Lyrics += "\n"
				}
				result.Lyrics += text
			}
		}
		collectAudio(obj, result)
	}
}

func collectAudio(value any, result *MusicResult) {
	obj, ok := value.(map[string]any)
	if !ok {
		return
	}
	kind := strings.ToLower(strings.TrimSpace(firstString(obj, "type")))
	if kind != "audio" && kind != "" && !strings.Contains(strings.ToLower(firstString(obj, "mime_type", "mimeType")), "audio/") {
		return
	}
	encoded := firstString(obj, "data", "b64_json", "b64")
	if encoded == "" {
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return
	}
	// Interactions exposes output_audio as the convenience property for the
	// last generated block. Keep the last valid block when walking steps too.
	result.Audio = decoded
	result.MIMEType = firstString(obj, "mime_type", "mimeType")
}

func firstString(obj map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := obj[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
