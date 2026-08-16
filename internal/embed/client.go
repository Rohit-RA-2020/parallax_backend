// Package embed talks to an OpenAI-compatible /v1/embeddings endpoint.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const maxBatch = 32

// Client embeds text through a dedicated embeddings API.
type Client struct {
	BaseURL    string
	APIKey     string
	Model      string
	HTTPClient *http.Client
}

func NewClient(baseURL, apiKey, model string) *Client {
	return &Client{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Model:   model,
		HTTPClient: &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				Proxy:             http.ProxyFromEnvironment,
				ForceAttemptHTTP2: true,
				MaxIdleConns:      8,
				IdleConnTimeout:   90 * time.Second,
			},
		},
	}
}

// Endpoint joins a configured base URL with /embeddings.
func Endpoint(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return ""
	}
	const suffix = "/embeddings"
	if strings.HasSuffix(base, suffix) {
		return base
	}
	return base + suffix
}

type wireRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type wireResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Embed returns one vector per input, in the same order.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	out := make([][]float32, len(texts))
	for start := 0; start < len(texts); start += maxBatch {
		end := start + maxBatch
		if end > len(texts) {
			end = len(texts)
		}
		batch, err := c.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		if len(batch) != end-start {
			return nil, fmt.Errorf("embed: expected %d vectors, got %d", end-start, len(batch))
		}
		copy(out[start:end], batch)
	}
	return out, nil
}

func (c *Client) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	url := Endpoint(c.BaseURL)
	if url == "" {
		return nil, fmt.Errorf("embed: base_url is not configured")
	}
	if strings.TrimSpace(c.Model) == "" {
		return nil, fmt.Errorf("embed: model is not configured")
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return nil, fmt.Errorf("embed: api_key is not configured")
	}

	body, err := json.Marshal(wireRequest{Model: c.Model, Input: texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("embed: read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embed: http %d: %s", resp.StatusCode, compactBody(raw))
	}
	var parsed wireResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("embed: decode response: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, fmt.Errorf("embed: %s", parsed.Error.Message)
	}
	out := make([][]float32, len(texts))
	for _, item := range parsed.Data {
		if item.Index < 0 || item.Index >= len(out) {
			return nil, fmt.Errorf("embed: out-of-range vector index %d", item.Index)
		}
		if len(item.Embedding) == 0 {
			return nil, fmt.Errorf("embed: empty vector at index %d", item.Index)
		}
		out[item.Index] = item.Embedding
	}
	for i, vec := range out {
		if len(vec) == 0 {
			return nil, fmt.Errorf("embed: missing vector at index %d", i)
		}
	}
	return out, nil
}

func compactBody(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 400 {
		return s[:400] + "…"
	}
	if s == "" {
		return "empty body"
	}
	return s
}
