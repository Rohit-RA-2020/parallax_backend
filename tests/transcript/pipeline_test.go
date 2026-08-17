package transcript_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"parallax/internal/embed"
	"parallax/internal/ffmpeg"
	"parallax/internal/llm"
	"parallax/internal/projects"
	"parallax/internal/qdrant"
	. "parallax/internal/transcript"
)

func firstPayloadKind(points []map[string]any, kind string) map[string]any {
	for _, point := range points {
		payload, _ := point["payload"].(map[string]any)
		if payload["kind"] == kind {
			return payload
		}
	}
	return nil
}

type fakeASR struct {
	result ASRResult
}

func (f fakeASR) Transcribe(context.Context, string, ProgressFunc) (ASRResult, error) {
	return f.result, nil
}

type countingASR struct {
	result ASRResult
	n      int
}

func (c *countingASR) Transcribe(context.Context, string, ProgressFunc) (ASRResult, error) {
	c.n++
	return c.result, nil
}

func TestIndexerWritesTranscriptAndUpsertsEnglishVectors(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}

	store, err := projects.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.Create("Talk")
	if err != nil {
		t.Fatal(err)
	}
	mediaDir := filepath.Join(project.Dir, "media")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(mediaDir, "talk.mp4")
	cmd := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-i", "sine=f=440:d=1",
		"-f", "lavfi", "-i", "color=c=black:s=16x16:d=1",
		"-shortest", "-pix_fmt", "yuv420p", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
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
			upserted = append(upserted, payload.Points...)
			_, _ = w.Write([]byte(`{"result":{"status":"ok"}}`))
		case r.Method == http.MethodPut:
			_, _ = w.Write([]byte(`{"result":true}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/delete"):
			_, _ = w.Write([]byte(`{"result":true}`))
		default:
			http.Error(w, "unhandled "+r.Method+" "+r.URL.Path, http.StatusInternalServerError)
		}
	}))
	defer qdrantSrv.Close()

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
	defer embedSrv.Close()

	emb := embed.NewClient(embedSrv.URL+"/v1", "k", "m")
	emb.HTTPClient = embedSrv.Client()
	qd := qdrant.NewClient(qdrantSrv.URL, "")
	qd.HTTPClient = qdrantSrv.Client()

	idx := &Indexer{
		Projects:   store,
		Bins:       ffmpeg.Bins{FFmpeg: "ffmpeg", FFprobe: "ffprobe"},
		Whisper:    fakeASR{result: ASRResult{Language: "hi", Model: "turbo", Segments: []Segment{{Text: "धन्यवाद"}}, Words: []Word{{Start: 0, End: 1, Text: "धन्यवाद"}}}},
		Embeddings: emb,
		Qdrant:     qd,
		Completer: func() llm.Completer {
			return scriptedCompleter{reply: `["Thanks"]`}
		},
	}
	if err := idx.Index(context.Background(), project.ID, "media/talk.mp4"); err != nil {
		t.Fatal(err)
	}
	hash, err := projects.HashFile(src)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Load(project.Dir, hash)
	if err != nil || doc == nil {
		t.Fatalf("doc=%+v err=%v", doc, err)
	}
	if doc.Segments[0].TextEN != "Thanks" || doc.Words[0].Text != "धन्यवाद" {
		t.Fatalf("doc=%+v", doc)
	}
	payload := firstPayloadKind(upserted, KindTranscript)
	if payload == nil {
		t.Fatalf("upserted=%#v", upserted)
	}
	if payload["text_en"] != "Thanks" || payload["path"] != "media/talk.mp4" || payload["content_hash"] != hash || payload["kind"] != KindTranscript {
		t.Fatalf("payload=%#v", payload)
	}
	if idx.Statuses(project.ID)["media/talk.mp4"].State != StateReady {
		t.Fatalf("status=%+v", idx.Statuses(project.ID))
	}
}

func TestIndexerSkipsImages(t *testing.T) {
	store, err := projects.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.Create("Stills")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(project.Dir, "media")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "still.jpg"), []byte("jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx := &Indexer{Projects: store, Whisper: fakeASR{}}
	if err := idx.Index(context.Background(), project.ID, "media/still.jpg"); err != nil {
		t.Fatal(err)
	}
	if idx.Statuses(project.ID)["media/still.jpg"].State != StateSkipped {
		t.Fatalf("status=%+v", idx.Statuses(project.ID))
	}
}

func TestFindByAudioHash(t *testing.T) {
	dir := t.TempDir()
	doc := &Document{
		ContentHash: "aaa",
		Path:        "media/a.mp4",
		AudioHash:   "audio-1",
		Segments:    []Segment{{ID: "seg-0000", Text: "Hi", TextEN: "Hi"}},
	}
	if err := Save(dir, doc); err != nil {
		t.Fatal(err)
	}
	got, err := FindByAudioHash(dir, "audio-1")
	if err != nil || got == nil || got.ContentHash != "aaa" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	missing, err := FindByAudioHash(dir, "nope")
	if err != nil || missing != nil {
		t.Fatalf("missing=%+v err=%v", missing, err)
	}
}

func TestIndexerSkipsCompletedTranscript(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	store, err := projects.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.Create("Skip")
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(project.Dir, "media", "talk.mp4")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "sine=f=440:d=0.4", "-f", "lavfi", "-i", "color=c=black:s=16x16:d=0.4", "-shortest", "-pix_fmt", "yuv420p", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
	hash, err := projects.HashFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(project.Dir, &Document{
		ContentHash: hash,
		Path:        "media/talk.mp4",
		Embedded:    true,
		Segments:    []Segment{{ID: "seg-0000", Text: "Hi", TextEN: "Hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	asr := &countingASR{result: ASRResult{Language: "en", Segments: []Segment{{Text: "new"}}}}
	idx := &Indexer{Projects: store, Bins: ffmpeg.Bins{FFmpeg: "ffmpeg", FFprobe: "ffprobe"}, Whisper: asr}
	if err := idx.Index(context.Background(), project.ID, "media/talk.mp4"); err != nil {
		t.Fatal(err)
	}
	if asr.n != 0 {
		t.Fatalf("whisper ran %d times", asr.n)
	}
	if idx.Statuses(project.ID)["media/talk.mp4"].State != StateReady {
		t.Fatalf("status=%+v", idx.Statuses(project.ID))
	}
}

func TestIndexerKeepsTranscriptWhenQdrantFails(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	store, err := projects.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.Create("Fail")
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(project.Dir, "media", "talk.mp4")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "sine=f=440:d=0.4", "-f", "lavfi", "-i", "color=c=black:s=16x16:d=0.4", "-shortest", "-pix_fmt", "yuv420p", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
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
	idx := &Indexer{
		Projects:   store,
		Bins:       ffmpeg.Bins{FFmpeg: "ffmpeg", FFprobe: "ffprobe"},
		Whisper:    fakeASR{result: ASRResult{Language: "en", Segments: []Segment{{Text: "Hello"}}, Words: []Word{{Text: "Hello"}}}},
		Embeddings: emb,
		Qdrant:     client,
	}
	if err := idx.Index(context.Background(), project.ID, "media/talk.mp4"); err != nil {
		t.Fatal(err)
	}
	if idx.Statuses(project.ID)["media/talk.mp4"].State != StateIndexFailed {
		t.Fatalf("status=%+v", idx.Statuses(project.ID))
	}
	hash, _ := projects.HashFile(src)
	doc, err := Load(project.Dir, hash)
	if err != nil || doc == nil || doc.Segments[0].TextEN != "Hello" {
		t.Fatalf("doc=%+v err=%v", doc, err)
	}
}
