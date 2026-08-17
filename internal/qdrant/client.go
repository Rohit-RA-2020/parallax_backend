// Package qdrant is a small REST client for local Qdrant.
package qdrant

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to one Qdrant instance.
type Client struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:  strings.TrimSpace(apiKey),
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// CollectionName is a valid Qdrant collection for a project id.
func CollectionName(projectID string) string {
	id := strings.TrimSpace(projectID)
	var b strings.Builder
	b.WriteString("p_")
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	return b.String()
}

// PointID is a stable UUID derived from content hash + segment id.
func PointID(contentHash, segmentID string) string {
	sum := sha1.Sum([]byte("parallax:" + contentHash + ":" + segmentID))
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// Point is one searchable transcript segment.
type Point struct {
	ID      string         `json:"id"`
	Vector  []float32      `json:"vector"`
	Payload map[string]any `json:"payload"`
}

// Hit is one search result.
type Hit struct {
	ID      string         `json:"id"`
	Score   float64        `json:"score"`
	Payload map[string]any `json:"payload"`
}

func (c *Client) EnsureCollection(ctx context.Context, name string, dim int) error {
	if dim < 1 {
		return fmt.Errorf("qdrant: vector size must be positive")
	}
	status, body, err := c.do(ctx, http.MethodGet, "/collections/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	if status >= 200 && status < 300 {
		got, ok := collectionDim(body)
		if ok && got != dim {
			return fmt.Errorf("qdrant: collection %s has size %d, embedder returned %d; rebuild the collection", name, got, dim)
		}
		return nil
	}
	if status != http.StatusNotFound {
		return fmt.Errorf("qdrant: get collection: http %d: %s", status, compact(body))
	}
	payload := map[string]any{
		"vectors": map[string]any{"size": dim, "distance": "Cosine"},
	}
	status, body, err = c.do(ctx, http.MethodPut, "/collections/"+url.PathEscape(name), payload)
	if err != nil {
		return err
	}
	if status >= 300 {
		return fmt.Errorf("qdrant: create collection: http %d: %s", status, compact(body))
	}
	return nil
}

func (c *Client) Upsert(ctx context.Context, collection string, points []Point) error {
	if len(points) == 0 {
		return nil
	}
	status, body, err := c.do(ctx, http.MethodPut, "/collections/"+url.PathEscape(collection)+"/points?wait=true", map[string]any{
		"points": points,
	})
	if err != nil {
		return err
	}
	if status >= 300 {
		return fmt.Errorf("qdrant: upsert: http %d: %s", status, compact(body))
	}
	return nil
}

func (c *Client) DeleteByPath(ctx context.Context, collection, path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	return c.deleteFilter(ctx, collection, map[string]any{
		"must": []map[string]any{{
			"key":   "path",
			"match": map[string]any{"value": path},
		}},
	})
}

func (c *Client) DeleteByHash(ctx context.Context, collection, hash string) error {
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return nil
	}
	return c.deleteFilter(ctx, collection, map[string]any{
		"must": []map[string]any{{
			"key":   "content_hash",
			"match": map[string]any{"value": hash},
		}},
	})
}

// DeleteCollection drops a project's embedding collection. Missing collections are fine.
func (c *Client) DeleteCollection(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	status, body, err := c.do(ctx, http.MethodDelete, "/collections/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return nil
	}
	if status >= 300 {
		return fmt.Errorf("qdrant: delete collection: http %d: %s", status, compact(body))
	}
	return nil
}

func (c *Client) deleteFilter(ctx context.Context, collection string, filter map[string]any) error {
	status, body, err := c.do(ctx, http.MethodPost, "/collections/"+url.PathEscape(collection)+"/points/delete?wait=true", map[string]any{
		"filter": filter,
	})
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return nil
	}
	if status >= 300 {
		return fmt.Errorf("qdrant: delete: http %d: %s", status, compact(body))
	}
	return nil
}

func (c *Client) Search(ctx context.Context, collection string, vector []float32, paths []string, limit int) ([]Hit, error) {
	if limit < 1 {
		limit = 8
	}
	if limit > 50 {
		limit = 50
	}
	body := map[string]any{
		"vector":       vector,
		"limit":        limit,
		"with_payload": true,
	}
	if cleaned := cleanPaths(paths); len(cleaned) > 0 {
		should := make([]map[string]any, 0, len(cleaned))
		for _, path := range cleaned {
			should = append(should, map[string]any{
				"key":   "path",
				"match": map[string]any{"value": path},
			})
		}
		body["filter"] = map[string]any{"should": should}
	}
	status, raw, err := c.do(ctx, http.MethodPost, "/collections/"+url.PathEscape(collection)+"/points/search", body)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return []Hit{}, nil
	}
	if status >= 300 {
		return nil, fmt.Errorf("qdrant: search: http %d: %s", status, compact(raw))
	}
	var parsed struct {
		Result []Hit `json:"result"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("qdrant: decode search: %w", err)
	}
	if parsed.Result == nil {
		return []Hit{}, nil
	}
	return parsed.Result, nil
}

func (c *Client) do(ctx context.Context, method, path string, payload any) (int, []byte, error) {
	if strings.TrimSpace(c.BaseURL) == "" {
		return 0, nil, fmt.Errorf("qdrant: url is not configured")
	}
	var rdr io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.APIKey != "" {
		req.Header.Set("api-key", c.APIKey)
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("qdrant request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("qdrant: read response: %w", err)
	}
	return resp.StatusCode, raw, nil
}

func collectionDim(raw []byte) (int, bool) {
	var parsed struct {
		Result struct {
			Config struct {
				Params struct {
					Vectors struct {
						Size int `json:"size"`
					} `json:"vectors"`
				} `json:"params"`
			} `json:"config"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &parsed) != nil {
		return 0, false
	}
	if parsed.Result.Config.Params.Vectors.Size < 1 {
		return 0, false
	}
	return parsed.Result.Config.Params.Vectors.Size, true
}

func cleanPaths(paths []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func compact(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 400 {
		return s[:400] + "…"
	}
	if s == "" {
		return "empty body"
	}
	return s
}
