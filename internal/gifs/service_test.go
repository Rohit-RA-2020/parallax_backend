package gifs

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSearchBlendsProvidersAndFetchesSignedResult(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewTLSServer(mux)
	defer server.Close()

	mux.HandleFunc("/v1/gifs/search", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") != "giphy-key" || r.URL.Query().Get("q") != "celebrate" {
			t.Fatalf("unexpected GIPHY query: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"id": "g1", "title": "Happy dance", "images": map[string]any{"original": map[string]any{"mp4": server.URL + "/media/g1.mp4", "url": server.URL + "/media/g1.gif", "width": "640", "height": "360"}, "fixed_width": map[string]any{"url": server.URL + "/media/g1-preview.gif"}}},
		}})
	})
	mux.HandleFunc("/api/v1/klipy-key/gifs/search", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": []any{
			map[string]any{"id": "k1", "title": "Confetti", "file": map[string]any{"md": map[string]any{"mp4": map[string]any{"url": server.URL + "/media/k1.mp4"}, "gif": map[string]any{"url": server.URL + "/media/k1.gif"}}, "sm": map[string]any{"gif": map[string]any{"url": server.URL + "/media/k1-preview.gif"}}}},
		}}})
	})
	mux.HandleFunc("/media/g1.mp4", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = io.WriteString(w, "video")
	})

	var secret [32]byte
	secret[0] = 42
	service := newService(Config{GiphyAPIKey: "giphy-key", GiphyBaseURL: server.URL, KlipyAPIKey: "klipy-key", KlipyBaseURL: server.URL}, server.Client(), secret)
	response, err := service.Search(context.Background(), "celebrate", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 2 {
		t.Fatalf("got %d results", len(response.Results))
	}
	if response.Results[0].Provider != "giphy" || response.Results[1].Provider != "klipy" {
		t.Fatalf("unexpected blend: %#v", response.Results)
	}

	download, err := service.Fetch(context.Background(), response.Results[0].ImportRef)
	if err != nil {
		t.Fatal(err)
	}
	defer download.Body.Close()
	data, err := io.ReadAll(download.Body)
	if err != nil {
		t.Fatal(err)
	}
	if download.Name != "happy-dance-gif.mp4" || string(data) != "video" {
		t.Fatalf("unexpected download: %q %q", download.Name, data)
	}
}

func TestImportReferenceRejectsTampering(t *testing.T) {
	var secret [32]byte
	service := newService(Config{}, http.DefaultClient, secret)
	ref, err := service.sign(importPayload{Provider: "giphy", ID: "1", URL: "https://media.giphy.com/a.gif", Ext: ".gif"})
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(ref, ".")
	parts[0] = "e30"
	if _, err := service.verify(strings.Join(parts, ".")); err == nil {
		t.Fatal("expected tampered reference to fail")
	}
}

func TestSearchPageAdvancesProviderOffset(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("offset"); got != "3" {
			t.Fatalf("offset = %q, want 3", got)
		}
		items := make([]any, 3)
		for i := range items {
			id := string(rune('a' + i))
			items[i] = map[string]any{"id": id, "title": id, "images": map[string]any{"original": map[string]any{"url": serverURL(r) + "/" + id + ".gif"}, "fixed_width": map[string]any{"url": serverURL(r) + "/" + id + ".gif"}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	defer server.Close()
	var secret [32]byte
	service := newService(Config{GiphyAPIKey: "key", GiphyBaseURL: server.URL}, server.Client(), secret)
	response, err := service.SearchPage(context.Background(), "more", 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 3 || !response.HasMore || response.NextOffset != 6 {
		t.Fatalf("unexpected page: %#v", response)
	}
}

func serverURL(r *http.Request) string {
	return "https://" + r.Host
}
