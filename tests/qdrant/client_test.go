package qdrant_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	. "parallax/internal/qdrant"
)

func TestCollectionNameAndPointID(t *testing.T) {
	if CollectionName("f16f-abc") != "p_f16f_abc" {
		t.Fatalf("name=%s", CollectionName("f16f-abc"))
	}
	a := PointID("hash", "seg-0001")
	b := PointID("hash", "seg-0001")
	c := PointID("hash", "seg-0002")
	if a != b || a == c || !strings.Contains(a, "-") {
		t.Fatalf("ids %s %s %s", a, b, c)
	}
}

func TestSearchFiltersPaths(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/points/search") {
			t.Fatalf("path=%s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": []map[string]any{{
				"id": "p1", "score": 0.91,
				"payload": map[string]any{"path": "media/talk.mp4", "text_en": "Thanks", "start": 4.1, "end": 6.0},
			}},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	c.HTTPClient = srv.Client()
	hits, err := c.Search(context.Background(), "p_demo", []float32{0.1, 0.2}, []string{"media/talk.mp4"}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Payload["text_en"] != "Thanks" {
		t.Fatalf("hits=%+v", hits)
	}
	filter := got["filter"].(map[string]any)
	should := filter["should"].([]any)
	match := should[0].(map[string]any)["match"].(map[string]any)
	if match["value"] != "media/talk.mp4" {
		t.Fatalf("filter=%#v", got["filter"])
	}
}

func TestDeleteCollectionIgnoresMissing(t *testing.T) {
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		http.Error(w, "missing", http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "")
	c.HTTPClient = srv.Client()
	if err := c.DeleteCollection(context.Background(), "p_demo"); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodDelete || path != "/collections/p_demo" {
		t.Fatalf("%s %s", method, path)
	}
}

func TestEnsureCollectionCreatesMissing(t *testing.T) {
	created := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.Error(w, "missing", http.StatusNotFound)
			return
		}
		if r.Method == http.MethodPut {
			created = true
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			vectors := body["vectors"].(map[string]any)
			if vectors["size"] != float64(3) {
				t.Fatalf("body=%#v", body)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"result":true}`))
			return
		}
		t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "qk")
	c.HTTPClient = srv.Client()
	if err := c.EnsureCollection(context.Background(), "p_demo", 3); err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("collection not created")
	}
}
