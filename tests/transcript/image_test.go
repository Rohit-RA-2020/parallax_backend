package transcript_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"parallax/internal/embed"
	"parallax/internal/llm"
	"parallax/internal/projects"
	"parallax/internal/qdrant"
	. "parallax/internal/transcript"
)

const tinyJPEG = "/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAIBAQEBAQIBAQECAgICAgQDAgICAgUEBAMEBgUGBgYFBgYGBwkIBgcJBwYGCAsICQoKCgoKBggLDAsKDAkKCgr/2wBDAQICAgICAgUDAwUKBwYHCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgr/wAARCAABAAEDASIAAhEBAxEB/8QAHwAAAQUBAQEBAQEAAAAAAAAAAAECAwQFBgcICQoL/8QAtRAAAgEDAwIEAwUFBAQAAAF9AQIDAAQRBRIhMUEGE1FhByJxFDKBkaEII0KxwRVS0fAkM2JyggkKJhcYGRolJicoKSo0NTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqDhIWGh4iJipKTlJWWl5iZmqKjpKWmp6ipqrKztLW2t7i5usLDxMXGx8jJytLT1NXW19jZ2uHi4+Tl5ufo6erx8vP09fb3+Pn6/8QAHwEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoL/8QAtREAAgECBAQDBAcFBAQAAQJ3AAECAxEEBSExBhJBUQdhcRMiMoEIFEKRobHBCSMzUvAVYnLRChYkNOEl8RcYGRomJygpKjU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6goOEhYaHiImKkpOUlZaXmJmaoqOkpaanqKmqsrO0tba3uLm6wsPExcbHyMnK0tPU1dbX2Nna4uPk5ebn6Onq8vP09fb3+Pn6/9oADAMBAAIRAxEAPwD4vooor+Uz/fw//9k="

type visionCompleter struct {
	reply  string
	calls  int
	images int
	text   string
}

func (c *visionCompleter) Complete(_ context.Context, req llm.Request) (string, error) {
	c.calls++
	for _, msg := range req.Messages {
		if strings.TrimSpace(msg.Content) != "" {
			c.text = msg.Content
		}
		c.images += len(msg.Images)
	}
	return c.reply, nil
}

func writeTinyJPEG(t *testing.T, path string) {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(tinyJPEG)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func imageIndexHarness(t *testing.T) (*Indexer, projects.Project, *[]map[string]any, *visionCompleter) {
	t.Helper()
	store, err := projects.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.Create("Stills")
	if err != nil {
		t.Fatal(err)
	}
	var upserted []map[string]any
	qdrantSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/collections/"):
			http.Error(w, "missing", http.StatusNotFound)
		case r.Method == http.MethodPut && strings.HasSuffix(strings.Split(r.URL.Path, "?")[0], "/points"):
			body, _ := io.ReadAll(r.Body)
			var payload struct {
				Points []map[string]any `json:"points"`
			}
			_ = json.Unmarshal(body, &payload)
			upserted = payload.Points
			_, _ = w.Write([]byte(`{"result":{"status":"ok"}}`))
		case r.Method == http.MethodPut:
			_, _ = w.Write([]byte(`{"result":true}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/delete"):
			_, _ = w.Write([]byte(`{"result":true}`))
		default:
			http.Error(w, "unhandled "+r.Method+" "+r.URL.Path, http.StatusInternalServerError)
		}
	}))
	t.Cleanup(qdrantSrv.Close)

	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var data []map[string]any
		for i := range req.Input {
			data = append(data, map[string]any{"index": i, "embedding": []float32{0.1, 0.2, 0.3}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(embedSrv.Close)

	emb := embed.NewClient(embedSrv.URL+"/v1", "k", "m")
	emb.HTTPClient = embedSrv.Client()
	qd := qdrant.NewClient(qdrantSrv.URL, "")
	qd.HTTPClient = qdrantSrv.Client()
	vision := &visionCompleter{reply: "Night alley, magenta neon, wet pavement reflections."}
	idx := &Indexer{
		Projects:   store,
		Embeddings: emb,
		Qdrant:     qd,
		Completer:  func() llm.Completer { return vision },
	}
	return idx, project, &upserted, vision
}

func TestIndexerCaptionsAndEmbedsImages(t *testing.T) {
	idx, project, upserted, vision := imageIndexHarness(t)
	src := filepath.Join(project.Dir, "media", "neon-alley.jpg")
	writeTinyJPEG(t, src)
	idx.SetImageHint(project.ID, "media/neon-alley.jpg", "cinematic neon alley at night")

	if err := idx.Index(context.Background(), project.ID, "media/neon-alley.jpg"); err != nil {
		t.Fatal(err)
	}
	if vision.calls != 1 || vision.images != 1 {
		t.Fatalf("vision calls=%d images=%d", vision.calls, vision.images)
	}
	if !strings.Contains(vision.text, "cinematic neon alley") {
		t.Fatalf("caption prompt=%q", vision.text)
	}
	hash, err := projects.HashFile(src)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := LoadImage(project.Dir, hash)
	if err != nil || doc == nil {
		t.Fatalf("doc=%+v err=%v", doc, err)
	}
	if doc.TextEN != vision.reply || doc.Prompt != "cinematic neon alley at night" || doc.Path != "media/neon-alley.jpg" {
		t.Fatalf("doc=%+v", doc)
	}
	if len(*upserted) != 1 {
		t.Fatalf("upserted=%#v", *upserted)
	}
	payload := (*upserted)[0]["payload"].(map[string]any)
	if payload["kind"] != KindImage || payload["path"] != "media/neon-alley.jpg" || payload["name"] != "neon-alley.jpg" || payload["text_en"] != vision.reply || payload["content_hash"] != hash {
		t.Fatalf("payload=%#v", payload)
	}
	if idx.Statuses(project.ID)["media/neon-alley.jpg"].State != StateReady {
		t.Fatalf("status=%+v", idx.Statuses(project.ID))
	}
}

func TestIndexerSkipsCompletedImageCaption(t *testing.T) {
	idx, project, _, vision := imageIndexHarness(t)
	src := filepath.Join(project.Dir, "media", "still.jpg")
	writeTinyJPEG(t, src)
	hash, err := projects.HashFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveImage(project.Dir, &ImageCaption{
		ContentHash: hash,
		Path:        "media/still.jpg",
		Name:        "still.jpg",
		TextEN:      "A red car at dusk",
		Embedded:    true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Index(context.Background(), project.ID, "media/still.jpg"); err != nil {
		t.Fatal(err)
	}
	if vision.calls != 0 {
		t.Fatalf("captioner ran %d times", vision.calls)
	}
	if idx.Statuses(project.ID)["media/still.jpg"].State != StateReady {
		t.Fatalf("status=%+v", idx.Statuses(project.ID))
	}
}

func TestIndexerKeepsImageCaptionWhenQdrantFails(t *testing.T) {
	store, err := projects.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.Create("Fail")
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(project.Dir, "media", "still.jpg")
	writeTinyJPEG(t, src)
	qd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.Error(w, "missing", http.StatusNotFound)
			return
		}
		if strings.Contains(r.URL.Path, "/points") && !strings.Contains(r.URL.Path, "/delete") {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"result":true}`))
	}))
	defer qd.Close()
	embSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{0.1, 0.2}}}})
	}))
	defer embSrv.Close()
	emb := embed.NewClient(embSrv.URL+"/v1", "k", "m")
	emb.HTTPClient = embSrv.Client()
	client := qdrant.NewClient(qd.URL, "")
	client.HTTPClient = qd.Client()
	vision := &visionCompleter{reply: "A red car at dusk on a wet highway."}
	idx := &Indexer{
		Projects:   store,
		Embeddings: emb,
		Qdrant:     client,
		Completer:  func() llm.Completer { return vision },
	}
	if err := idx.Index(context.Background(), project.ID, "media/still.jpg"); err != nil {
		t.Fatal(err)
	}
	if idx.Statuses(project.ID)["media/still.jpg"].State != StateIndexFailed {
		t.Fatalf("status=%+v", idx.Statuses(project.ID))
	}
	hash, _ := projects.HashFile(src)
	doc, err := LoadImage(project.Dir, hash)
	if err != nil || doc == nil || doc.TextEN != vision.reply {
		t.Fatalf("doc=%+v err=%v", doc, err)
	}
}

func TestSearchImagesFiltersKind(t *testing.T) {
	var got map[string]any
	qd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/points/search") {
			http.Error(w, "unhandled", http.StatusInternalServerError)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": []map[string]any{{
				"id": "p1", "score": 0.88,
				"payload": map[string]any{"kind": "image", "path": "media/still.jpg", "name": "still.jpg", "text_en": "Red car"},
			}},
		})
	}))
	defer qd.Close()
	embSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{0.2, 0.1}}}})
	}))
	defer embSrv.Close()
	emb := embed.NewClient(embSrv.URL+"/v1", "k", "m")
	emb.HTTPClient = embSrv.Client()
	client := qdrant.NewClient(qd.URL, "")
	client.HTTPClient = qd.Client()
	idx := &Indexer{Embeddings: emb, Qdrant: client}
	hits, err := idx.SearchImages(context.Background(), "demo", "red car", nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Payload["path"] != "media/still.jpg" {
		t.Fatalf("hits=%+v", hits)
	}
	filter := got["filter"].(map[string]any)
	must := filter["must"].([]any)
	if must[0].(map[string]any)["key"] != "kind" {
		t.Fatalf("filter=%#v", filter)
	}
	match := must[0].(map[string]any)["match"].(map[string]any)
	if match["value"] != KindImage {
		t.Fatalf("filter=%#v", filter)
	}
}

func TestSearchTranscriptExcludesImages(t *testing.T) {
	var got map[string]any
	qd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{"result": []map[string]any{}})
	}))
	defer qd.Close()
	embSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{0.2, 0.1}}}})
	}))
	defer embSrv.Close()
	emb := embed.NewClient(embSrv.URL+"/v1", "k", "m")
	emb.HTTPClient = embSrv.Client()
	client := qdrant.NewClient(qd.URL, "")
	client.HTTPClient = qd.Client()
	idx := &Indexer{Embeddings: emb, Qdrant: client}
	if _, err := idx.Search(context.Background(), "demo", "thanks", nil, 4); err != nil {
		t.Fatal(err)
	}
	filter := got["filter"].(map[string]any)
	mustNot := filter["must_not"].([]any)
	match := mustNot[0].(map[string]any)["match"].(map[string]any)
	if match["value"] != KindImage {
		t.Fatalf("filter=%#v", filter)
	}
}
