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

func TestPlanSceneWindowsUsesCutsAndSplitsLongTakes(t *testing.T) {
	windows := PlanSceneWindows([]float64{0.1, 2.0, 2.2, 10.0}, 20)
	if len(windows) < 4 {
		t.Fatalf("windows=%+v", windows)
	}
	if windows[0].Start != 0 || windows[0].End < 1.9 || windows[0].End > 2.1 {
		t.Fatalf("first shot=%+v", windows[0])
	}
	var longTake bool
	for _, win := range windows {
		if win.Start >= 9.9 && win.End-win.Start <= 3.6 {
			longTake = true
		}
	}
	if !longTake {
		t.Fatalf("expected interval samples after 10s, got %+v", windows)
	}
}

func TestPlanSceneWindowsIntervalWhenNoCuts(t *testing.T) {
	windows := PlanSceneWindows(nil, 12)
	if len(windows) < 3 {
		t.Fatalf("windows=%+v", windows)
	}
	if windows[0].Start != 0 {
		t.Fatalf("start=%v", windows[0].Start)
	}
	if windows[len(windows)-1].End != 12 {
		t.Fatalf("end=%v", windows[len(windows)-1].End)
	}
}

func TestIndexerCaptionsVideoScenesAndAttachesSpeech(t *testing.T) {
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
	project, err := store.Create("Broll")
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(project.Dir, "media", "broll.mp4")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-i", "color=c=red:s=64x64:d=1",
		"-f", "lavfi", "-i", "sine=f=440:d=1",
		"-f", "lavfi", "-i", "color=c=blue:s=64x64:d=1",
		"-f", "lavfi", "-i", "sine=f=440:d=1",
		"-filter_complex", "[0:v][2:v]concat=n=2:v=1:a=0[v];[1:a][3:a]concat=n=2:v=0:a=1[a]",
		"-map", "[v]", "-map", "[a]",
		"-pix_fmt", "yuv420p", src)
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
			data = append(data, map[string]any{"index": i, "embedding": []float32{0.2, 0.1, 0.4}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer embedSrv.Close()
	emb := embed.NewClient(embedSrv.URL+"/v1", "k", "m")
	emb.HTTPClient = embedSrv.Client()
	qd := qdrant.NewClient(qdrantSrv.URL, "")
	qd.HTTPClient = qdrantSrv.Client()
	vision := &visionCompleter{reply: "A solid color frame, first red then blue."}

	idx := &Indexer{
		Projects:   store,
		Bins:       ffmpeg.Bins{FFmpeg: "ffmpeg", FFprobe: "ffprobe"},
		Whisper:    fakeASR{result: ASRResult{Language: "en", Model: "turbo", Segments: []Segment{{ID: "seg-0000", Start: 0.2, End: 0.8, Text: "Hello there", TextEN: "Hello there"}}}},
		Embeddings: emb,
		Qdrant:     qd,
		Completer:  func() llm.Completer { return vision },
	}
	if err := idx.IndexDescribe(context.Background(), project.ID, "media/broll.mp4"); err != nil {
		t.Fatal(err)
	}
	if vision.calls < 1 || vision.images < 1 {
		t.Fatalf("vision calls=%d images=%d", vision.calls, vision.images)
	}
	hash, err := projects.HashFile(src)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := LoadVideoScenes(project.Dir, hash)
	if err != nil || doc == nil || len(doc.Scenes) == 0 {
		t.Fatalf("doc=%+v err=%v", doc, err)
	}
	if strings.TrimSpace(doc.Scenes[0].TextEN) == "" {
		t.Fatalf("missing caption: %+v", doc.Scenes[0])
	}
	if doc.Scenes[0].SpokenEN != "Hello there" && !sceneHasSpoken(doc.Scenes, "Hello there") {
		t.Fatalf("expected overlapping speech, scenes=%+v", doc.Scenes)
	}
	scenePayload := firstPayloadKind(upserted, KindVideoScene)
	if scenePayload == nil || scenePayload["path"] != "media/broll.mp4" {
		t.Fatalf("scene upsert=%#v", upserted)
	}
	if idx.Statuses(project.ID)["media/broll.mp4"].State != StateReady {
		t.Fatalf("status=%+v", idx.Statuses(project.ID))
	}
}

func TestIndexerSkipsScenesWhenTranscriptExists(t *testing.T) {
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
	src := filepath.Join(project.Dir, "media", "talk.mp4")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-i", "sine=f=440:d=1",
		"-f", "lavfi", "-i", "color=c=black:s=16x16:d=1",
		"-shortest", "-pix_fmt", "yuv420p", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
	embeds := 0
	qdrantSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/collections/"):
			http.Error(w, "missing", http.StatusNotFound)
		case r.Method == http.MethodPut && strings.HasSuffix(strings.Split(r.URL.Path, "?")[0], "/points"):
			_, _ = w.Write([]byte(`{"result":{"status":"ok"}}`))
		case r.Method == http.MethodPut, r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"result":true}`))
		default:
			http.Error(w, "unhandled", http.StatusInternalServerError)
		}
	}))
	defer qdrantSrv.Close()
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		embeds++
		var data []map[string]any
		for i := range req.Input {
			data = append(data, map[string]any{"index": i, "embedding": []float32{0.1, 0.2}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer embedSrv.Close()
	emb := embed.NewClient(embedSrv.URL+"/v1", "k", "m")
	emb.HTTPClient = embedSrv.Client()
	qd := qdrant.NewClient(qdrantSrv.URL, "")
	qd.HTTPClient = qdrantSrv.Client()
	vision := &visionCompleter{reply: "should not run"}
	var segs []Segment
	for i := 0; i < 10; i++ {
		segs = append(segs, Segment{Text: "Hello there", TextEN: "Hello there"})
	}
	idx := &Indexer{
		Projects:   store,
		Bins:       ffmpeg.Bins{FFmpeg: "ffmpeg", FFprobe: "ffprobe"},
		Whisper:    streamASR{result: ASRResult{Language: "en", Model: "turbo", Segments: segs}},
		Embeddings: emb,
		Qdrant:     qd,
		Completer:  func() llm.Completer { return vision },
	}
	if err := idx.Index(context.Background(), project.ID, "media/talk.mp4"); err != nil {
		t.Fatal(err)
	}
	if vision.calls != 0 {
		t.Fatalf("described spoken video: calls=%d", vision.calls)
	}
	st := idx.Statuses(project.ID)["media/talk.mp4"]
	if !st.CanDescribe || st.State != StateReady {
		t.Fatalf("status=%+v", st)
	}
	hash, err := projects.HashFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if scenes, err := LoadVideoScenes(project.Dir, hash); err != nil || scenes != nil {
		t.Fatalf("scenes should be skipped: %+v err=%v", scenes, err)
	}
	if embeds < 1 {
		t.Fatalf("expected transcript embeddings, got %d", embeds)
	}
}

type streamASR struct {
	result ASRResult
}

func (s streamASR) Transcribe(context.Context, string, ProgressFunc) (ASRResult, error) {
	return s.result, nil
}

func (s streamASR) TranscribeStream(_ context.Context, _ string, _ ProgressFunc, onSeg SegmentFunc) (ASRResult, error) {
	for _, seg := range s.result.Segments {
		if onSeg != nil {
			onSeg(seg, nil)
		}
	}
	return s.result, nil
}

func sceneHasSpoken(scenes []VideoScene, want string) bool {
	for _, scene := range scenes {
		if scene.SpokenEN == want {
			return true
		}
	}
	return false
}

func TestIndexerScenesMutedVideoWithoutWhisper(t *testing.T) {
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
	project, err := store.Create("Mute")
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(project.Dir, "media", "mute.mp4")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "color=c=green:s=32x32:d=1", "-pix_fmt", "yuv420p", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
	qdrantSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			http.Error(w, "missing", http.StatusNotFound)
		default:
			_, _ = w.Write([]byte(`{"result":true}`))
		}
	}))
	defer qdrantSrv.Close()
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{0.1, 0.2}}}})
	}))
	defer embedSrv.Close()
	emb := embed.NewClient(embedSrv.URL+"/v1", "k", "m")
	emb.HTTPClient = embedSrv.Client()
	qd := qdrant.NewClient(qdrantSrv.URL, "")
	qd.HTTPClient = qdrantSrv.Client()
	vision := &visionCompleter{reply: "A flat green frame."}
	idx := &Indexer{
		Projects:   store,
		Bins:       ffmpeg.Bins{FFmpeg: "ffmpeg", FFprobe: "ffprobe"},
		Embeddings: emb,
		Qdrant:     qd,
		Completer:  func() llm.Completer { return vision },
	}
	if err := idx.Index(context.Background(), project.ID, "media/mute.mp4"); err != nil {
		t.Fatal(err)
	}
	hash, _ := projects.HashFile(src)
	doc, err := LoadVideoScenes(project.Dir, hash)
	if err != nil || doc == nil || len(doc.Scenes) == 0 || doc.Scenes[0].TextEN != vision.reply {
		t.Fatalf("doc=%+v err=%v", doc, err)
	}
}

func TestSearchScenesFiltersKind(t *testing.T) {
	var got map[string]any
	qd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": []map[string]any{{
				"id": "s1", "score": 0.8,
				"payload": map[string]any{"kind": "video_scene", "path": "media/broll.mp4", "start": 1.0, "end": 4.0},
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
	hits, err := idx.SearchScenes(context.Background(), "demo", "kitchen", nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Payload["path"] != "media/broll.mp4" {
		t.Fatalf("hits=%+v", hits)
	}
	must := got["filter"].(map[string]any)["must"].([]any)
	if must[0].(map[string]any)["key"] != "kind" {
		t.Fatalf("filter=%#v", got["filter"])
	}
	if must[0].(map[string]any)["match"].(map[string]any)["value"] != KindVideoScene {
		t.Fatalf("filter=%#v", got["filter"])
	}
}
