package gifs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func (s *Service) searchGiphy(ctx context.Context, query string, limit, offset int) ([]candidate, error) {
	u, _ := url.Parse(s.cfg.GiphyBaseURL + "/v1/gifs/search")
	q := u.Query()
	q.Set("api_key", s.cfg.GiphyAPIKey)
	q.Set("q", query)
	q.Set("limit", strconv.Itoa(limit))
	q.Set("offset", strconv.Itoa(offset))
	q.Set("rating", "pg")
	q.Set("lang", "en")
	u.RawQuery = q.Encode()
	var response struct {
		Data []map[string]any `json:"data"`
	}
	if err := s.getJSON(ctx, u.String(), &response); err != nil {
		return nil, err
	}
	out := make([]candidate, 0, len(response.Data))
	for rank, item := range response.Data {
		download := stringPath(item, "images.original.mp4", "images.original.url", "images.downsized.url")
		preview := stringPath(item, "images.fixed_width.webp", "images.fixed_width.url", "images.original.url")
		if download == "" || preview == "" {
			continue
		}
		ext := extFromURL(download, ".gif")
		if strings.Contains(download, ".mp4") {
			ext = ".mp4"
		}
		out = append(out, candidate{ID: stringPath(item, "id"), Title: stringPath(item, "title", "slug"), Provider: "giphy", Preview: preview, Download: download, Ext: ext, Width: intPath(item, "images.original.width"), Height: intPath(item, "images.original.height"), Rank: rank})
	}
	return out, nil
}

func (s *Service) searchKlipy(ctx context.Context, query string, limit, offset int) ([]candidate, error) {
	// KLIPY's native API places the key in the path and wraps results in data.data.
	u, _ := url.Parse(s.cfg.KlipyBaseURL + "/api/v1/" + url.PathEscape(s.cfg.KlipyAPIKey) + "/gifs/search")
	q := u.Query()
	q.Set("q", query)
	q.Set("per_page", strconv.Itoa(limit))
	q.Set("limit", strconv.Itoa(limit))
	q.Set("page", strconv.Itoa(offset/limit+1))
	u.RawQuery = q.Encode()
	var raw map[string]any
	if err := s.getJSON(ctx, u.String(), &raw); err != nil {
		return nil, err
	}
	items := findItems(raw)
	out := make([]candidate, 0, len(items))
	for rank, item := range items {
		download := stringPath(item, "file.hd.mp4.url", "file.md.mp4.url", "file.sm.mp4.url", "file.hd.gif.url", "file.md.gif.url", "file.sm.gif.url", "media_formats.mp4.url", "media_formats.gif.url")
		preview := stringPath(item, "file.sm.webp.url", "file.sm.gif.url", "file.md.gif.url", "media_formats.tinygif.url", "media_formats.gif.url")
		if download == "" || preview == "" {
			continue
		}
		ext := extFromURL(download, ".gif")
		if strings.Contains(download, ".mp4") {
			ext = ".mp4"
		}
		out = append(out, candidate{ID: stringPath(item, "id", "slug"), Title: stringPath(item, "title", "name", "slug"), Provider: "klipy", Preview: preview, Download: download, Ext: ext, Width: intPath(item, "file.hd.width", "file.md.width", "media_formats.gif.dims.0"), Height: intPath(item, "file.hd.height", "file.md.height", "media_formats.gif.dims.1"), Rank: rank})
	}
	return out, nil
}

func findItems(root map[string]any) []map[string]any {
	for _, path := range []string{"data.data", "data.items", "results", "items", "data"} {
		value := mapPath(root, path)
		array, ok := value.([]any)
		if !ok {
			continue
		}
		items := make([]map[string]any, 0, len(array))
		for _, entry := range array {
			if item, ok := entry.(map[string]any); ok {
				items = append(items, item)
			}
		}
		return items
	}
	return []map[string]any{}
}

func (s *Service) getJSON(ctx context.Context, endpoint string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("provider returned %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return fmt.Errorf("decode provider response: %w", err)
	}
	return nil
}
