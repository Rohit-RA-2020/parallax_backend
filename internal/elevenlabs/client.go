// Package elevenlabs contains the small, provider-specific HTTP client used by
// Director audio generation tools. It intentionally uses only the Go standard
// library so the media agent does not need an ElevenLabs SDK or Python.
package elevenlabs

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
	"strconv"
	"strings"
	"time"
)

const (
	DefaultBaseURL         = "https://api.elevenlabs.io"
	DefaultTTSModel        = "eleven_v3"
	DefaultMusicModel      = "music_v2"
	DefaultSFXModel        = "eleven_text_to_sound_v2"
	DefaultMaxResponseSize = 256 << 20
	maxErrorBody           = 64 << 10
)

type Client struct {
	APIKey           string
	BaseURL          string
	HTTPClient       *http.Client
	MaxResponseBytes int64
}

type SpeechRequest struct {
	VoiceID           string         `json:"-"`
	Text              string         `json:"text"`
	ModelID           string         `json:"model_id,omitempty"`
	LanguageCode      string         `json:"language_code,omitempty"`
	VoiceSettings     map[string]any `json:"voice_settings,omitempty"`
	Seed              *int64         `json:"seed,omitempty"`
	PreviousText      string         `json:"previous_text,omitempty"`
	NextText          string         `json:"next_text,omitempty"`
	TextNormalization string         `json:"apply_text_normalization,omitempty"`
}

type Alignment struct {
	Characters          []string  `json:"characters"`
	CharacterStartTimes []float64 `json:"character_start_times_seconds"`
	CharacterEndTimes   []float64 `json:"character_end_times_seconds"`
}

type SpeechResponse struct {
	AudioBase64         string     `json:"audio_base64"`
	Alignment           *Alignment `json:"alignment"`
	NormalizedAlignment *Alignment `json:"normalized_alignment"`
}

type MusicRequest struct {
	Prompt                  string          `json:"prompt,omitempty"`
	CompositionPlan         json.RawMessage `json:"composition_plan,omitempty"`
	MusicLengthMS           *int            `json:"music_length_ms,omitempty"`
	ModelID                 string          `json:"model_id,omitempty"`
	Seed                    *int64          `json:"seed,omitempty"`
	ForceInstrumental       bool            `json:"force_instrumental,omitempty"`
	FinetuneID              string          `json:"finetune_id,omitempty"`
	RespectSectionDurations *bool           `json:"respect_sections_durations,omitempty"`
	StoreForInpainting      bool            `json:"store_for_inpainting,omitempty"`
	SignWithC2PA            bool            `json:"sign_with_c2pa,omitempty"`
}

type SoundEffectRequest struct {
	Text            string   `json:"text"`
	Loop            bool     `json:"loop,omitempty"`
	DurationSeconds *float64 `json:"duration_seconds,omitempty"`
	PromptInfluence *float64 `json:"prompt_influence,omitempty"`
	ModelID         string   `json:"model_id,omitempty"`
}

type AudioResult struct {
	SongID      string
	Bytes       int64
	ContentType string
}

// ProviderError preserves the ElevenLabs HTTP status and bounded response
// body so callers can distinguish authentication, validation, rate-limit, and
// provider failures without exposing unbounded provider output.
type ProviderError struct {
	StatusCode int
	Body       string
}

func (e *ProviderError) Error() string {
	if e == nil {
		return "elevenlabs: provider error"
	}
	message := strings.TrimSpace(e.Body)
	if message == "" {
		message = http.StatusText(e.StatusCode)
	}
	return fmt.Sprintf("elevenlabs: http %d: %s", e.StatusCode, message)
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

func (c *Client) GenerateSpeechWithTimestamps(ctx context.Context, req SpeechRequest, outputFormat string) (SpeechResponse, error) {
	if strings.TrimSpace(req.VoiceID) == "" {
		return SpeechResponse{}, errors.New("elevenlabs: voice_id is required")
	}
	if strings.TrimSpace(req.Text) == "" {
		return SpeechResponse{}, errors.New("elevenlabs: text is required")
	}
	if strings.TrimSpace(req.ModelID) == "" {
		req.ModelID = DefaultTTSModel
	}
	if strings.TrimSpace(outputFormat) == "" {
		outputFormat = "mp3_44100_128"
	}
	path := "/v1/text-to-speech/" + url.PathEscape(req.VoiceID) + "/with-timestamps"
	path += "?output_format=" + url.QueryEscape(outputFormat)
	body, err := json.Marshal(req)
	if err != nil {
		return SpeechResponse{}, err
	}
	_, raw, err := c.doJSON(ctx, http.MethodPost, path, body)
	if err != nil {
		return SpeechResponse{}, err
	}
	var out SpeechResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return SpeechResponse{}, fmt.Errorf("elevenlabs: decode speech response: %w", err)
	}
	if strings.TrimSpace(out.AudioBase64) == "" {
		return SpeechResponse{}, errors.New("elevenlabs: speech response did not contain audio")
	}
	return out, nil
}

func (c *Client) ComposeMusic(ctx context.Context, req MusicRequest, outputFormat string, dst io.Writer) (AudioResult, error) {
	if strings.TrimSpace(req.Prompt) == "" && len(bytes.TrimSpace(req.CompositionPlan)) == 0 {
		return AudioResult{}, errors.New("elevenlabs: music requires prompt or composition_plan")
	}
	if strings.TrimSpace(req.Prompt) != "" && len(bytes.TrimSpace(req.CompositionPlan)) > 0 {
		return AudioResult{}, errors.New("elevenlabs: prompt and composition_plan are mutually exclusive")
	}
	if strings.TrimSpace(req.ModelID) == "" {
		req.ModelID = DefaultMusicModel
	}
	if req.MusicLengthMS != nil && (*req.MusicLengthMS < 3000 || *req.MusicLengthMS > 600000) {
		return AudioResult{}, errors.New("elevenlabs: music_length_ms must be between 3000 and 600000")
	}
	if strings.TrimSpace(outputFormat) == "" {
		outputFormat = "auto"
	}
	body, err := json.Marshal(req)
	if err != nil {
		return AudioResult{}, err
	}
	return c.streamAudio(ctx, "/v1/music?output_format="+url.QueryEscape(outputFormat), body, dst)
}

func (c *Client) GenerateSoundEffect(ctx context.Context, req SoundEffectRequest, outputFormat string, dst io.Writer) (AudioResult, error) {
	if strings.TrimSpace(req.Text) == "" {
		return AudioResult{}, errors.New("elevenlabs: sound effect text is required")
	}
	if req.DurationSeconds != nil && (*req.DurationSeconds < 0.5 || *req.DurationSeconds > 30) {
		return AudioResult{}, errors.New("elevenlabs: duration_seconds must be between 0.5 and 30")
	}
	if req.PromptInfluence != nil && (*req.PromptInfluence < 0 || *req.PromptInfluence > 1) {
		return AudioResult{}, errors.New("elevenlabs: prompt_influence must be between 0 and 1")
	}
	if strings.TrimSpace(req.ModelID) == "" {
		req.ModelID = DefaultSFXModel
	}
	if strings.TrimSpace(outputFormat) == "" {
		outputFormat = "mp3_44100_128"
	}
	body, err := json.Marshal(req)
	if err != nil {
		return AudioResult{}, err
	}
	return c.streamAudio(ctx, "/v1/sound-generation?output_format="+url.QueryEscape(outputFormat), body, dst)
}

func (c *Client) doJSON(ctx context.Context, method, path string, body []byte) (*http.Response, []byte, error) {
	resp, err := c.do(ctx, method, path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	limit := c.maxResponseBytes()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return resp, nil, fmt.Errorf("elevenlabs: read response: %w", err)
	}
	if int64(len(raw)) > limit {
		return resp, nil, fmt.Errorf("elevenlabs: response exceeds %d bytes", limit)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, nil, providerError(resp.StatusCode, raw)
	}
	return resp, raw, nil
}

func (c *Client) streamAudio(ctx context.Context, path string, body []byte, dst io.Writer) (AudioResult, error) {
	if dst == nil {
		return AudioResult{}, errors.New("elevenlabs: output writer is required")
	}
	resp, err := c.do(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return AudioResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return AudioResult{}, providerError(resp.StatusCode, raw)
	}
	count := &countingWriter{dst: dst, max: c.maxResponseBytes()}
	if _, err := io.Copy(count, resp.Body); err != nil {
		return AudioResult{}, fmt.Errorf("elevenlabs: stream audio: %w", err)
	}
	return AudioResult{
		SongID:      firstHeader(resp, "song-id", "song_id"),
		Bytes:       count.n,
		ContentType: resp.Header.Get("Content-Type"),
	}, nil
}

func firstHeader(resp *http.Response, names ...string) string {
	if resp == nil {
		return ""
	}
	for _, name := range names {
		if value := strings.TrimSpace(resp.Header.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return nil, errors.New("elevenlabs: API key is not configured")
	}
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("xi-api-key", c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "audio/*, application/json")
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("elevenlabs request: %w", err)
	}
	return resp, nil
}

func (c *Client) maxResponseBytes() int64 {
	if c != nil && c.MaxResponseBytes > 0 {
		return c.MaxResponseBytes
	}
	return DefaultMaxResponseSize
}

type countingWriter struct {
	dst io.Writer
	max int64
	n   int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	if w.max > 0 && w.n+int64(len(p)) > w.max {
		return 0, fmt.Errorf("response exceeds %d bytes", w.max)
	}
	n, err := w.dst.Write(p)
	w.n += int64(n)
	return n, err
}

func providerError(status int, body []byte) error {
	message := strings.TrimSpace(string(body))
	if len(message) > 2000 {
		message = message[:2000] + "…"
	}
	return &ProviderError{StatusCode: status, Body: message}
}

func DecodeAudioBase64(encoded string, maxBytes int64) ([]byte, error) {
	if strings.TrimSpace(encoded) == "" {
		return nil, errors.New("elevenlabs: empty audio payload")
	}
	decodedLen := base64.StdEncoding.DecodedLen(len(encoded))
	if maxBytes > 0 && int64(decodedLen) > maxBytes {
		return nil, fmt.Errorf("elevenlabs: decoded audio exceeds %d bytes", maxBytes)
	}
	out := make([]byte, decodedLen)
	n, err := base64.StdEncoding.Decode(out, []byte(encoded))
	if err != nil {
		return nil, fmt.Errorf("elevenlabs: decode audio: %w", err)
	}
	return out[:n], nil
}

func ParseIntHeader(resp *http.Response, name string) int64 {
	if resp == nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(resp.Header.Get(name)), 10, 64)
	return n
}
