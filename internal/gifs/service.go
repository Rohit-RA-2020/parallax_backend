// Package gifs provides one normalized search and import surface over GIF providers.
package gifs

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxImportBytes = 64 << 20

type Config struct {
	GiphyAPIKey  string
	GiphyBaseURL string
	KlipyAPIKey  string
	KlipyBaseURL string
}

type Result struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Provider  string `json:"provider"`
	Preview   string `json:"preview_url"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	ImportRef string `json:"import_ref"`
}

type SearchResponse struct {
	Query      string   `json:"query"`
	Results    []Result `json:"results"`
	Providers  []string `json:"providers"`
	Offset     int      `json:"offset"`
	NextOffset int      `json:"next_offset,omitempty"`
	HasMore    bool     `json:"has_more"`
}

type importPayload struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Ext      string `json:"ext"`
}

type Download struct {
	Name        string
	ContentType string
	Body        io.ReadCloser
}

type Service struct {
	cfg    Config
	client *http.Client
	secret [32]byte
}

func New(cfg Config) *Service {
	var secret [32]byte
	_, _ = rand.Read(secret[:])
	return newService(cfg, &http.Client{Timeout: 15 * time.Second}, secret)
}

func newService(cfg Config, client *http.Client, secret [32]byte) *Service {
	cfg.GiphyBaseURL = strings.TrimRight(defaultString(cfg.GiphyBaseURL, "https://api.giphy.com"), "/")
	cfg.KlipyBaseURL = strings.TrimRight(defaultString(cfg.KlipyBaseURL, "https://api.klipy.com"), "/")
	return &Service{cfg: cfg, client: client, secret: secret}
}

func (s *Service) Configured() bool {
	return strings.TrimSpace(s.cfg.GiphyAPIKey) != "" || strings.TrimSpace(s.cfg.KlipyAPIKey) != ""
}

func (s *Service) Search(ctx context.Context, query string, limit int) (SearchResponse, error) {
	return s.SearchPage(ctx, query, limit, 0)
}

func (s *Service) SearchPage(ctx context.Context, query string, limit, offset int) (SearchResponse, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return SearchResponse{Query: query, Results: []Result{}, Providers: []string{}, Offset: 0}, nil
	}
	if len([]rune(query)) > 50 {
		return SearchResponse{}, errors.New("GIF search is limited to 50 characters")
	}
	if limit < 1 || limit > 50 {
		limit = 24
	}
	if offset < 0 {
		offset = 0
	}

	type providerResult struct {
		name  string
		items []candidate
		err   error
	}
	ch := make(chan providerResult, 2)
	count := 0
	if strings.TrimSpace(s.cfg.GiphyAPIKey) != "" {
		count++
	}
	if strings.TrimSpace(s.cfg.KlipyAPIKey) != "" {
		count++
	}
	if count == 0 {
		return SearchResponse{}, errors.New("GIF search is not configured; set GIPHY_API_KEY or KLIPY_API_KEY")
	}
	providerLimit := (limit + count - 1) / count
	page := offset / limit
	providerOffset := page * providerLimit
	if strings.TrimSpace(s.cfg.GiphyAPIKey) != "" {
		go func() {
			items, err := s.searchGiphy(ctx, query, providerLimit, providerOffset)
			ch <- providerResult{"giphy", items, err}
		}()
	}
	if strings.TrimSpace(s.cfg.KlipyAPIKey) != "" {
		go func() {
			items, err := s.searchKlipy(ctx, query, providerLimit, providerOffset)
			ch <- providerResult{"klipy", items, err}
		}()
	}

	byProvider := map[string][]candidate{}
	var errs []string
	hasMore := false
	for range count {
		got := <-ch
		if got.err != nil {
			errs = append(errs, got.name+": "+got.err.Error())
			continue
		}
		byProvider[got.name] = got.items
		if len(got.items) == providerLimit {
			hasMore = true
		}
	}
	if len(byProvider) == 0 {
		return SearchResponse{}, errors.New(strings.Join(errs, "; "))
	}

	// Reciprocal-rank fusion keeps both catalogs useful while promoting each
	// provider's strongest matches. Round-robin tie-breaking avoids provider bias.
	merged := fuse(byProvider, limit)
	results := make([]Result, 0, len(merged))
	for _, item := range merged {
		ref, err := s.sign(importPayload{Provider: item.Provider, ID: item.ID, Title: item.Title, URL: item.Download, Ext: item.Ext})
		if err != nil {
			continue
		}
		results = append(results, Result{ID: item.ID, Title: item.Title, Provider: item.Provider, Preview: item.Preview, Width: item.Width, Height: item.Height, ImportRef: ref})
	}
	providers := make([]string, 0, len(byProvider))
	for name := range byProvider {
		providers = append(providers, name)
	}
	sort.Strings(providers)
	nextOffset := 0
	if hasMore && len(results) > 0 {
		nextOffset = offset + limit
	}
	return SearchResponse{Query: query, Results: results, Providers: providers, Offset: offset, NextOffset: nextOffset, HasMore: nextOffset > 0}, nil
}

func (s *Service) Fetch(ctx context.Context, ref string) (Download, error) {
	payload, err := s.verify(ref)
	if err != nil {
		return Download{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, payload.URL, nil)
	if err != nil {
		return Download{}, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return Download{}, fmt.Errorf("download GIF: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return Download{}, fmt.Errorf("download GIF: provider returned %s", resp.Status)
	}
	if resp.ContentLength > maxImportBytes {
		_ = resp.Body.Close()
		return Download{}, errors.New("GIF is larger than 64 MiB")
	}
	ext := payload.Ext
	if ext != ".gif" && ext != ".mp4" && ext != ".webm" {
		ext = ".gif"
	}
	name := safeSlug(payload.Title)
	if name == "" {
		name = payload.Provider + "-" + safeSlug(payload.ID)
	}
	if name == "" {
		name = "gif"
	}
	return Download{Name: name + "-gif" + ext, ContentType: resp.Header.Get("Content-Type"), Body: &limitedReadCloser{Reader: &maxReader{r: resp.Body, remaining: maxImportBytes}, closer: resp.Body}}, nil
}

type limitedReadCloser struct {
	io.Reader
	closer io.Closer
}

func (r *limitedReadCloser) Close() error { return r.closer.Close() }

type maxReader struct {
	r         io.Reader
	remaining int64
}

func (r *maxReader) Read(p []byte) (int, error) {
	if r.remaining > 0 {
		if int64(len(p)) > r.remaining {
			p = p[:r.remaining]
		}
		n, err := r.r.Read(p)
		r.remaining -= int64(n)
		return n, err
	}
	var probe [1]byte
	n, err := r.r.Read(probe[:])
	if n > 0 {
		return 0, errors.New("GIF is larger than 64 MiB")
	}
	return 0, err
}

type candidate struct {
	ID, Title, Provider, Preview, Download, Ext string
	Width, Height, Rank                         int
}

func fuse(groups map[string][]candidate, limit int) []candidate {
	providers := []string{"giphy", "klipy"}
	seen := map[string]bool{}
	out := make([]candidate, 0, limit)
	for rank := 0; len(out) < limit; rank++ {
		added := false
		for _, provider := range providers {
			items := groups[provider]
			if rank >= len(items) {
				continue
			}
			item := items[rank]
			key := canonicalMediaURL(item.Download)
			if key == "" {
				key = provider + ":" + item.ID
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, item)
			added = true
			if len(out) == limit {
				break
			}
		}
		if !added {
			break
		}
	}
	return out
}

func canonicalMediaURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host) + u.Path
}

func (s *Service) sign(payload importPayload) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, s.secret[:])
	_, _ = mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (s *Service) verify(ref string) (importPayload, error) {
	parts := strings.Split(ref, ".")
	if len(parts) != 2 {
		return importPayload{}, errors.New("invalid GIF import reference")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return importPayload{}, errors.New("invalid GIF import reference")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return importPayload{}, errors.New("invalid GIF import reference")
	}
	mac := hmac.New(sha256.New, s.secret[:])
	_, _ = mac.Write(raw)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return importPayload{}, errors.New("invalid GIF import reference")
	}
	var payload importPayload
	if json.Unmarshal(raw, &payload) != nil || payload.URL == "" {
		return importPayload{}, errors.New("invalid GIF import reference")
	}
	u, err := url.Parse(payload.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return importPayload{}, errors.New("invalid GIF media URL")
	}
	return payload, nil
}

func safeSlug(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}
func parseInt(value string) int { n, _ := strconv.Atoi(value); return n }
func extFromURL(raw, fallback string) string {
	u, err := url.Parse(raw)
	if err == nil {
		if ext := strings.ToLower(filepath.Ext(u.Path)); ext == ".gif" || ext == ".mp4" || ext == ".webm" {
			return ext
		}
	}
	return fallback
}

// mapPath walks maps using a dotted path.
func mapPath(root map[string]any, path string) any {
	var current any = root
	for _, part := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[part]
	}
	return current
}
func stringPath(root map[string]any, paths ...string) string {
	for _, path := range paths {
		if value, ok := mapPath(root, path).(string); ok && value != "" {
			return value
		}
	}
	return ""
}
func intPath(root map[string]any, paths ...string) int {
	for _, path := range paths {
		switch value := mapPath(root, path).(type) {
		case float64:
			return int(value)
		case string:
			if n := parseInt(value); n > 0 {
				return n
			}
		}
	}
	return 0
}
