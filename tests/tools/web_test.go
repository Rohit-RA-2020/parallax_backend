package tools_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	. "parallax/internal/tools"
)

func TestSearchWebSendsExaRequestAndReturnsSources(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "exa-test-key" {
			t.Fatalf("api key=%q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["query"] != "latest camera technology" || body["numResults"] != float64(2) {
			t.Fatalf("body=%#v", body)
		}
		contents := body["contents"].(map[string]any)
		highlights := contents["highlights"].(map[string]any)
		if highlights["maxCharacters"] != float64(1200) {
			t.Fatalf("highlights=%#v", highlights)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"Camera guide","url":"https://example.com/cameras","id":"doc-1","publishedDate":"2026-08-15T00:00:00Z","author":"A. Writer","favicon":"https://example.com/favicon.ico","highlights":["A useful camera fact."]}],"requestId":"req-1","resolvedSearchType":"auto"}`))
	}))
	defer server.Close()

	reg := NewRegistry()
	RegisterWeb(reg, WebEnv{APIKey: "exa-test-key", BaseURL: server.URL})
	res := reg.Execute(context.Background(), "search_web", `{"query":"latest camera technology","num_results":2,"max_characters":1200}`)
	if !res.OK {
		t.Fatal(res.Error)
	}
	out := res.Output.(map[string]any)
	if out["request_id"] != "req-1" {
		t.Fatalf("output=%#v", out)
	}
	results := out["results"].([]map[string]any)
	if len(results) != 1 || results[0]["url"] != "https://example.com/cameras" {
		t.Fatalf("results=%#v", results)
	}
}

func TestSearchWebSupportsFullText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		contents := body["contents"].(map[string]any)
		text := contents["text"].(map[string]any)
		if text["maxCharacters"] != float64(5000) {
			t.Fatalf("text=%#v", text)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"Page","url":"https://example.com/page","text":"Full page markdown"}]}`))
	}))
	defer server.Close()

	reg := NewRegistry()
	RegisterWeb(reg, WebEnv{APIKey: "key", BaseURL: server.URL})
	res := reg.Execute(context.Background(), "search_web", `{"query":"page","content_mode":"text","max_characters":5000}`)
	if !res.OK {
		t.Fatal(res.Error)
	}
	out := res.Output.(map[string]any)
	results := out["results"].([]map[string]any)
	if results[0]["text"] != "Full page markdown" {
		t.Fatalf("results=%#v", results)
	}
}

func TestSearchWebRequiresAPIKey(t *testing.T) {
	reg := NewRegistry()
	RegisterWeb(reg, WebEnv{})
	res := reg.Execute(context.Background(), "search_web", `{"query":"anything"}`)
	if res.OK || res.Error == "" {
		t.Fatalf("result=%+v", res)
	}
}
