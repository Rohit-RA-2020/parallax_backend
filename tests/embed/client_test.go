package embed_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	. "parallax/internal/embed"
)

func TestEmbedBatchesAndPreservesOrder(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer emb-key" {
			t.Fatalf("auth=%s", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatal(err)
		}
		if req.Model != "text-embedding-3-small" {
			t.Fatalf("model=%s", req.Model)
		}
		seen = append(seen, req.Input...)
		var data []map[string]any
		for i := range req.Input {
			data = append(data, map[string]any{"index": i, "embedding": []float32{float32(i + 1), 0}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/v1", "emb-key", "text-embedding-3-small")
	c.HTTPClient = srv.Client()
	vecs, err := c.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 || vecs[0][0] != 1 || vecs[1][0] != 2 {
		t.Fatalf("vecs=%v", vecs)
	}
	if stringsJoin(seen) != "a|b" {
		t.Fatalf("seen=%v", seen)
	}
}

func TestEndpoint(t *testing.T) {
	if Endpoint("https://api.openai.com/v1/") != "https://api.openai.com/v1/embeddings" {
		t.Fatal(Endpoint("https://api.openai.com/v1/"))
	}
	if Endpoint("https://host/v1/embeddings") != "https://host/v1/embeddings" {
		t.Fatal(Endpoint("https://host/v1/embeddings"))
	}
}

func stringsJoin(in []string) string {
	out := ""
	for i, s := range in {
		if i > 0 {
			out += "|"
		}
		out += s
	}
	return out
}
