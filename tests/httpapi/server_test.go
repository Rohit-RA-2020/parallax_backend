package httpapi_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"parallax/internal/agent"
	"parallax/internal/config"
	"parallax/internal/embed"
	. "parallax/internal/httpapi"
	"parallax/internal/llm"
	"parallax/internal/projects"
	"parallax/internal/qdrant"
	"parallax/internal/tools"
	"parallax/internal/transcript"
)

type fakeProvider struct {
	deltas []llm.Delta
	seen   *llm.Request
}

type parallelTitleProvider struct {
	titleStarted chan llm.Request
	answerStart  chan struct{}
}

func (p *parallelTitleProvider) Complete(_ context.Context, req llm.Request) (string, error) {
	p.titleStarted <- req
	<-p.answerStart
	return `"Mute Highway Audio."`, nil
}

func (p *parallelTitleProvider) Stream(_ context.Context, _ llm.Request) (<-chan llm.Delta, error) {
	select {
	case req := <-p.titleStarted:
		if req.ReasoningEffort != llm.ThinkingEffortLow {
			return nil, errors.New("title request did not use low reasoning")
		}
	case <-time.After(time.Second):
		return nil, errors.New("title request was not started in parallel")
	}
	close(p.answerStart)
	ch := make(chan llm.Delta, 1)
	ch <- llm.Delta{Content: "Done", FinishReason: "stop"}
	close(ch)
	return ch, nil
}

func TestFirstMessageGeneratesAndPersistsChatTitleInParallel(t *testing.T) {
	provider := &parallelTitleProvider{titleStarted: make(chan llm.Request, 1), answerStart: make(chan struct{})}
	s := testServer(t, provider)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	project, err := s.Projects.Create("Titles")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(ts.URL+"/v1/agent/chat", "application/json", strings.NewReader(`{"project_id":"`+project.ID+`","message":"Please mute the highway clip audio"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s", resp.Status, raw)
	}
	if !bytes.Contains(raw, []byte(`event: chat_title`)) || !bytes.Contains(raw, []byte(`Mute Highway Audio`)) {
		t.Fatalf("missing generated title event: %s", raw)
	}
	chats, err := s.Projects.ListChats(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 1 || chats[0].Title != "Mute Highway Audio" {
		t.Fatalf("chats=%+v", chats)
	}
}

func (f fakeProvider) Stream(_ context.Context, req llm.Request) (<-chan llm.Delta, error) {
	if f.seen != nil {
		*f.seen = req
	}
	ch := make(chan llm.Delta, len(f.deltas))
	for _, d := range f.deltas {
		ch <- d
	}
	close(ch)
	return ch, nil
}

func TestChatPassesThinkingEffort(t *testing.T) {
	var seen llm.Request
	s := testServer(t, fakeProvider{
		deltas: []llm.Delta{{Content: "ok", FinishReason: "stop"}},
		seen:   &seen,
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	project, err := s.Projects.Create("Thinking")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"project_id":"` + project.ID + `","message":"hi","thinking_effort":"low"}`
	resp, err := http.Post(ts.URL+"/v1/agent/chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%s", resp.Status)
	}
	if seen.ReasoningEffort != llm.ThinkingEffortLow {
		t.Fatalf("reasoning effort=%q", seen.ReasoningEffort)
	}
}

func TestVisualReviewPersistsRevisionLinkedResult(t *testing.T) {
	s := testServer(t, fakeProvider{deltas: []llm.Delta{{Content: `{"findings":[]}`, FinishReason: "stop"}}})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	project, err := s.Projects.Create("Review")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"revision":0,"mode":"full"}`
	resp, err := http.Post(ts.URL+"/v1/projects/"+project.ID+"/visual-review", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%s", resp.Status)
	}
	var result struct {
		Revision int    `json:"revision"`
		Status   string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "degraded" || result.Revision != 0 {
		t.Fatalf("result=%+v", result)
	}
	get, err := http.Get(ts.URL + "/v1/projects/" + project.ID + "/visual-reviews/0")
	if err != nil {
		t.Fatal(err)
	}
	defer get.Body.Close()
	if get.StatusCode != http.StatusOK {
		t.Fatalf("get status=%s", get.Status)
	}
}

func TestChatAcceptsAttachedImage(t *testing.T) {
	var seen llm.Request
	s := testServer(t, fakeProvider{
		deltas: []llm.Delta{{Content: "warm tungsten", FinishReason: "stop"}},
		seen:   &seen,
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	project, err := s.Projects.Create("Refs")
	if err != nil {
		t.Fatal(err)
	}
	jpeg := "/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAIBAQEBAQIBAQECAgICAgQDAgICAgUEBAMEBgUGBgYFBgYGBwkIBgcJBwYGCAsICQoKCgoKBggLDAsKDAkKCgr/2wBDAQICAgICAgUDAwUKBwYHCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgr/wAARCAABAAEDASIAAhEBAxEB/8QAHwAAAQUBAQEBAQEAAAAAAAAAAAECAwQFBgcICQoL/8QAtRAAAgEDAwIEAwUFBAQAAAF9AQIDAAQRBRIhMUEGE1FhByJxFDKBkaEII0KxwRVS0fAkM2JyggkKFhcYGRolJicoKSo0NTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqDhIWGh4iJipKTlJWWl5iZmqKjpKWmp6ipqrKztLW2t7i5usLDxMXGx8jJytLT1NXW19jZ2uHi4+Tl5ufo6erx8vP09fb3+Pn6/8QAHwEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoL/8QAtREAAgECBAQDBAcFBAQAAQJ3AAECAxEEBSExBhJBUQdhcRMiMoEIFEKRobHBCSMzUvAVYnLRChYkNOEl8RcYGRomJygpKjU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6goOEhYaHiImKkpOUlZaXmJmaoqOkpaanqKmqsrO0tba3uLm6wsPExcbHyMnK0tPU1dbX2Nna4+Tl5ufo6erx8vP09fb3+Pn6/9oADAMBAAIRAxEAPwD4vooor+Uz/fw//9k="
	body := `{"project_id":"` + project.ID + `","message":"match this look","images":[{"name":"ref.jpg","mime":"image/jpeg","data":"` + jpeg + `"}]}`
	resp, err := http.Post(ts.URL+"/v1/agent/chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%s", resp.Status)
	}
	if len(seen.Messages) == 0 {
		t.Fatal("no messages sent to the model")
	}
	var user llm.Message
	for _, msg := range seen.Messages {
		if msg.Role == llm.RoleUser {
			user = msg
		}
	}
	if user.Content != "match this look" || len(user.Images) != 1 || user.Images[0].Data == "" {
		t.Fatalf("user=%+v", user)
	}
	if !strings.HasPrefix(user.Images[0].Path, "media/") {
		t.Fatalf("attachment was not promoted to the media bin: %q", user.Images[0].Path)
	}
	if _, err := os.Stat(filepath.Join(project.Dir, filepath.FromSlash(user.Images[0].Path))); err != nil {
		t.Fatal(err)
	}
	media, err := s.Projects.ListMedia(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range media {
		if item.Path == user.Images[0].Path {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("attachment %q is not visible in the media bin", user.Images[0].Path)
	}
}

func TestChatRegistersGenerateImage(t *testing.T) {
	var seen llm.Request
	s := testServer(t, fakeProvider{
		deltas: []llm.Delta{{Content: "ok", FinishReason: "stop"}},
		seen:   &seen,
	})
	s.GeminiAPIKey = "gemini-test"
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	project, err := s.Projects.Create("Stills")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"project_id":"` + project.ID + `","message":"generate a title card"}`
	resp, err := http.Post(ts.URL+"/v1/agent/chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%s", resp.Status)
	}
	names := map[string]bool{}
	for _, tool := range seen.Tools {
		names[tool.Function.Name] = true
	}
	if !names["generate_image"] {
		t.Fatalf("tools=%v", names)
	}
}

func testServer(t *testing.T, p llm.ChatProvider) *Server {
	t.Helper()
	dir := t.TempDir()
	reg := tools.NewRegistry()
	projectStore, err := projects.NewStore(filepath.Join(dir, "projects"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Settings: config.NewStore(filepath.Join(dir, "settings.json"), []config.LLM{{
			ID:      "default",
			BaseURL: config.DefaultBaseURL,
			APIKey:  "test-key",
			Model:   config.DefaultModel,
		}}),
		Sessions:  agent.NewStore(),
		Tools:     reg,
		Projects:  projectStore,
		MaxIters:  4,
		Workspace: dir,
		NewLLM:    func(config.LLM) llm.ChatProvider { return p },
	}
	uploads, err := NewUploadManager(UploadManagerConfig{
		Workspace: dir, Projects: projectStore, MaxSize: 64 << 30,
		Validate: func(context.Context, string, string) error { return nil },
		OnReady: func(projectID string, media projects.Media, uploadMs int64) {
			if s.Indexer != nil {
				s.Indexer.NoteUpload(projectID, media.Path, uploadMs)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Uploads = uploads
	t.Cleanup(uploads.Close)
	return s
}

func tusUpload(t *testing.T, baseURL, projectID, name string, body []byte) string {
	t.Helper()
	meta := func(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/uploads/", nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", strconv.Itoa(len(body)))
	req.Header.Set("Upload-Metadata", "project_id "+meta(projectID)+",filename "+meta(name)+",filetype "+meta("application/octet-stream"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create upload: %s %s", resp.Status, raw)
	}
	location := resp.Header.Get("Location")
	if strings.HasPrefix(location, "/") {
		location = baseURL + location
	}
	patch, _ := http.NewRequest(http.MethodPatch, location, bytes.NewReader(body))
	patch.Header.Set("Tus-Resumable", "1.0.0")
	patch.Header.Set("Upload-Offset", "0")
	patch.Header.Set("Content-Type", "application/offset+octet-stream")
	resp, err = http.DefaultClient.Do(patch)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("patch upload: %s %s", resp.Status, raw)
	}
	id := strings.TrimSuffix(location, "/")
	id = id[strings.LastIndex(id, "/")+1:]
	for i := 0; i < 200; i++ {
		status, err := http.Get(baseURL + "/v1/upload-status/" + id)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			State string `json:"state"`
			Error string `json:"error"`
			Media *struct {
				ContentURL string `json:"content_url"`
			} `json:"media"`
		}
		_ = json.NewDecoder(status.Body).Decode(&result)
		status.Body.Close()
		if result.State == "ready" && result.Media != nil {
			return result.Media.ContentURL
		}
		if result.State == "failed" {
			t.Fatalf("finalize upload: %s", result.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("upload did not finalize")
	return ""
}

func TestSearchMediaReturnsIndexHits(t *testing.T) {
	s := testServer(t, fakeProvider{})
	var gotQuery []string
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotQuery = req.Input
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{0.2, 0.1}}}})
	}))
	defer embedSrv.Close()
	qdrantSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/points/search") {
			http.Error(w, "unhandled", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": []map[string]any{{
				"id": "p1", "score": 0.87,
				"payload": map[string]any{"kind": "image", "path": "media/neon-alley.jpg", "name": "neon-alley.jpg", "text_en": "Night alley with magenta neon"},
			}},
		})
	}))
	defer qdrantSrv.Close()
	emb := embed.NewClient(embedSrv.URL+"/v1", "k", "m")
	emb.HTTPClient = embedSrv.Client()
	qd := qdrant.NewClient(qdrantSrv.URL, "")
	qd.HTTPClient = qdrantSrv.Client()
	s.Indexer = &transcript.Indexer{Projects: s.Projects, Embeddings: emb, Qdrant: qd}

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/projects", "application/json", strings.NewReader(`{"name":"Demo"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	resp, err = http.Get(ts.URL + "/v1/projects/" + created.ID + "/media/search?q=neon+alley")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%s", resp.Status)
	}
	var body struct {
		Query   string `json:"query"`
		Results []struct {
			Path   string  `json:"path"`
			Kind   string  `json:"kind"`
			Score  float64 `json:"score"`
			TextEN string  `json:"text_en"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Query != "neon alley" || len(body.Results) != 1 || body.Results[0].Path != "media/neon-alley.jpg" || body.Results[0].Kind != "image" {
		t.Fatalf("body=%+v", body)
	}
	if len(gotQuery) != 1 || gotQuery[0] != "neon alley" {
		t.Fatalf("embedded=%v", gotQuery)
	}
}

func TestListMediaIncludesTranscriptStatus(t *testing.T) {
	s := testServer(t, fakeProvider{})
	s.Indexer = &transcript.Indexer{Projects: s.Projects}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/projects", "application/json", strings.NewReader(`{"name":"Demo"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	tusUpload(t, ts.URL, created.ID, "talk.mp4", []byte("video-bytes"))
	s.Indexer.Mark(created.ID, "media/talk.mp4", transcript.StateTranscribing, "")

	resp, err = http.Get(ts.URL + "/v1/projects/" + created.ID + "/media")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var listed struct {
		Media []struct {
			Path       string `json:"path"`
			Transcript *struct {
				State   string `json:"state"`
				Timings *struct {
					UploadMs int64 `json:"upload_ms"`
				} `json:"timings"`
			} `json:"transcript"`
		} `json:"media"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Media) != 1 || listed.Media[0].Transcript == nil || listed.Media[0].Transcript.State != transcript.StateTranscribing {
		t.Fatalf("listed=%+v", listed)
	}
	if listed.Media[0].Transcript.Timings == nil || listed.Media[0].Transcript.Timings.UploadMs < 1 {
		t.Fatalf("upload timings=%+v", listed.Media[0].Transcript)
	}
}

func TestDeleteProjectRemovesWorkspaceAndIndex(t *testing.T) {
	s := testServer(t, fakeProvider{})
	deleted := ""
	qdrantSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || !strings.Contains(r.URL.Path, "/collections/") {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		deleted = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":true}`))
	}))
	defer qdrantSrv.Close()
	s.Indexer = &transcript.Indexer{
		Projects: s.Projects,
		Qdrant:   qdrant.NewClient(qdrantSrv.URL, ""),
	}
	s.Indexer.Qdrant.HTTPClient = qdrantSrv.Client()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/projects", "application/json", strings.NewReader(`{"name":"Demo"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	project, err := s.Projects.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Projects.SaveUpload(created.ID, "talk.mp4", strings.NewReader("video-bytes")); err != nil {
		t.Fatal(err)
	}
	chat, err := s.Projects.CreateChat(created.ID, "Talk")
	if err != nil {
		t.Fatal(err)
	}
	s.Sessions.Remember(&agent.Session{ID: chat.ID, ProjectID: created.ID})
	s.Indexer.Mark(created.ID, "media/talk.mp4", transcript.StateReady, "")

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/projects/"+created.ID, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("delete %s %s", resp.Status, raw)
	}
	if _, err := s.Projects.Get(created.ID); !errors.Is(err, projects.ErrNotFound) {
		t.Fatalf("project still listed: %v", err)
	}
	if _, err := os.Stat(project.Dir); !os.IsNotExist(err) {
		t.Fatalf("workspace still exists: %v", err)
	}
	if _, ok := s.Sessions.Get(chat.ID); ok {
		t.Fatal("chat session still in memory")
	}
	if len(s.Indexer.Statuses(created.ID)) != 0 {
		t.Fatalf("index status=%+v", s.Indexer.Statuses(created.ID))
	}
	if !strings.Contains(deleted, qdrant.CollectionName(created.ID)) {
		t.Fatalf("collection path=%q", deleted)
	}

	resp, err = http.Get(ts.URL + "/v1/projects/" + created.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete status=%s", resp.Status)
	}
	req, _ = http.NewRequest(http.MethodDelete, ts.URL+"/v1/projects/"+created.ID, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete status=%s", resp.Status)
	}
}

func TestUploadRejectsOversizeFile(t *testing.T) {
	s := testServer(t, fakeProvider{})
	s.Uploads.Close()
	uploads, err := NewUploadManager(UploadManagerConfig{Workspace: s.Workspace, Projects: s.Projects, MaxSize: 64, Validate: func(context.Context, string, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	s.Uploads = uploads
	defer uploads.Close()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/projects", "application/json", strings.NewReader(`{"name":"Demo"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	meta := base64.StdEncoding.EncodeToString
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/uploads/", nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", "200")
	req.Header.Set("Upload-Metadata", "project_id "+meta([]byte(created.ID))+",filename "+meta([]byte("huge.mp4")))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%s body=%s", resp.Status, raw)
	}
}

func TestProjectUploadAndServe(t *testing.T) {
	s := testServer(t, fakeProvider{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/projects", "application/json", strings.NewReader(`{"name":"Demo"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatal(resp.Status)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	contentURL := tusUpload(t, ts.URL, created.ID, "clip.mp4", []byte("video-bytes"))
	resp, err = http.Get(ts.URL + contentURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	served, _ := io.ReadAll(resp.Body)
	if string(served) != "video-bytes" {
		t.Fatalf("served=%q", served)
	}

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+strings.Split(contentURL, "?")[0], nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("delete %s %s", resp.Status, raw)
	}
	resp, err = http.Get(ts.URL + "/v1/projects/" + created.ID + "/media")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var listed struct {
		Media []struct{} `json:"media"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Media) != 0 {
		t.Fatalf("listed=%+v", listed)
	}
}

func TestTusUploadResumesAndUsesSingleStoredObject(t *testing.T) {
	s := testServer(t, fakeProvider{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	project, err := s.Projects.Create("Resume")
	if err != nil {
		t.Fatal(err)
	}
	meta := func(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }
	create, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/uploads/", nil)
	create.Header.Set("Tus-Resumable", "1.0.0")
	create.Header.Set("Upload-Length", "11")
	create.Header.Set("Upload-Metadata", "project_id "+meta(project.ID)+",filename "+meta("resume.mp4"))
	resp, err := http.DefaultClient.Do(create)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatal(resp.Status)
	}
	location := resp.Header.Get("Location")
	if strings.HasPrefix(location, "/") {
		location = ts.URL + location
	}
	patch := func(offset string, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPatch, location, strings.NewReader(body))
		req.Header.Set("Tus-Resumable", "1.0.0")
		req.Header.Set("Upload-Offset", offset)
		req.Header.Set("Content-Type", "application/offset+octet-stream")
		got, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	resp = patch("0", "video")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Upload-Offset") != "5" {
		t.Fatalf("first patch=%s offset=%s", resp.Status, resp.Header.Get("Upload-Offset"))
	}
	head, _ := http.NewRequest(http.MethodHead, location, nil)
	head.Header.Set("Tus-Resumable", "1.0.0")
	resp, err = http.DefaultClient.Do(head)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Upload-Offset") != "5" {
		t.Fatalf("resume offset=%s", resp.Header.Get("Upload-Offset"))
	}
	resp = patch("0", "bad")
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("wrong offset status=%s", resp.Status)
	}
	resp = patch("5", "-bytes")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("final patch=%s", resp.Status)
	}
	id := location[strings.LastIndex(location, "/")+1:]
	for i := 0; i < 200; i++ {
		status, _ := http.Get(ts.URL + "/v1/upload-status/" + id)
		var result struct {
			State string `json:"state"`
		}
		_ = json.NewDecoder(status.Body).Decode(&result)
		status.Body.Close()
		if result.State == "ready" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mediaInfo, err := os.Stat(filepath.Join(project.Dir, "media", "resume.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	objects, err := os.ReadDir(filepath.Join(project.Dir, ".parallax", "objects"))
	if err != nil || len(objects) != 1 {
		t.Fatalf("objects=%v err=%v", objects, err)
	}
	objectInfo, err := os.Stat(filepath.Join(project.Dir, ".parallax", "objects", objects[0].Name()))
	if err != nil || !os.SameFile(mediaInfo, objectInfo) {
		t.Fatalf("media and history object should be hard links: %v", err)
	}

	legacy, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/projects/"+project.ID+"/media", strings.NewReader("legacy"))
	resp, err = http.DefaultClient.Do(legacy)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("legacy endpoint=%s", resp.Status)
	}
}

func TestTusOptionsExposeProtocolHeaders(t *testing.T) {
	s := testServer(t, fakeProvider{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodOptions, ts.URL+"/v1/uploads/", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if (resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK) || resp.Header.Get("Tus-Version") == "" {
		t.Fatalf("options=%s tus-version=%q", resp.Status, resp.Header.Get("Tus-Version"))
	}
	if !strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "Upload-Offset") {
		t.Fatalf("allow headers=%q", resp.Header.Get("Access-Control-Allow-Headers"))
	}
}

func TestTusUploadCanBeCancelled(t *testing.T) {
	s := testServer(t, fakeProvider{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	project, err := s.Projects.Create("Cancel")
	if err != nil {
		t.Fatal(err)
	}
	meta := func(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }
	create, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/uploads/", nil)
	create.Header.Set("Tus-Resumable", "1.0.0")
	create.Header.Set("Upload-Length", "100")
	create.Header.Set("Upload-Metadata", "project_id "+meta(project.ID)+",filename "+meta("cancel.mp4"))
	resp, err := http.DefaultClient.Do(create)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	location := resp.Header.Get("Location")
	if strings.HasPrefix(location, "/") {
		location = ts.URL + location
	}
	cancel, _ := http.NewRequest(http.MethodDelete, location, nil)
	cancel.Header.Set("Tus-Resumable", "1.0.0")
	resp, err = http.DefaultClient.Do(cancel)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("cancel=%s", resp.Status)
	}
	id := location[strings.LastIndex(location, "/")+1:]
	for i := 0; i < 100; i++ {
		status, _ := http.Get(ts.URL + "/v1/upload-status/" + id)
		var result struct {
			State string `json:"state"`
		}
		_ = json.NewDecoder(status.Body).Decode(&result)
		status.Body.Close()
		if result.State == "expired" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("cancelled upload did not become expired")
}

func TestTusUploadEnforcesActiveLimit(t *testing.T) {
	s := testServer(t, fakeProvider{})
	s.Uploads.Close()
	uploads, err := NewUploadManager(UploadManagerConfig{Workspace: s.Workspace, Projects: s.Projects, MaxSize: 1024, MaxActive: 1, MaxPerProject: 1, Validate: func(context.Context, string, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	s.Uploads = uploads
	defer uploads.Close()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	project, _ := s.Projects.Create("Limits")
	meta := func(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }
	create := func(name string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/uploads/", nil)
		req.Header.Set("Tus-Resumable", "1.0.0")
		req.Header.Set("Upload-Length", "100")
		req.Header.Set("Upload-Metadata", "project_id "+meta(project.ID)+",filename "+meta(name))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	first := create("one.mp4")
	first.Body.Close()
	if first.StatusCode != http.StatusCreated {
		t.Fatal(first.Status)
	}
	second := create("two.mp4")
	second.Body.Close()
	if second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second create=%s", second.Status)
	}
}

func TestTusUploadOffsetSurvivesServerRestart(t *testing.T) {
	s := testServer(t, fakeProvider{})
	project, _ := s.Projects.Create("Restart")
	ts := httptest.NewServer(s.Handler())
	meta := func(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }
	create, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/uploads/", nil)
	create.Header.Set("Tus-Resumable", "1.0.0")
	create.Header.Set("Upload-Length", "10")
	create.Header.Set("Upload-Metadata", "project_id "+meta(project.ID)+",filename "+meta("restart.mp4"))
	resp, err := http.DefaultClient.Do(create)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	path := resp.Header.Get("Location")
	path = path[strings.Index(path, "/v1/uploads/"):]
	patch, _ := http.NewRequest(http.MethodPatch, ts.URL+path, strings.NewReader("12345"))
	patch.Header.Set("Tus-Resumable", "1.0.0")
	patch.Header.Set("Upload-Offset", "0")
	patch.Header.Set("Content-Type", "application/offset+octet-stream")
	resp, err = http.DefaultClient.Do(patch)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	ts.Close()
	s.Uploads.Close()

	uploads, err := NewUploadManager(UploadManagerConfig{Workspace: s.Workspace, Projects: s.Projects, MaxSize: 1024, Validate: func(context.Context, string, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	s.Uploads = uploads
	defer uploads.Close()
	ts = httptest.NewServer(s.Handler())
	defer ts.Close()
	head, _ := http.NewRequest(http.MethodHead, ts.URL+path, nil)
	head.Header.Set("Tus-Resumable", "1.0.0")
	resp, err = http.DefaultClient.Do(head)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Upload-Offset") != "5" {
		t.Fatalf("restart head=%s offset=%s", resp.Status, resp.Header.Get("Upload-Offset"))
	}
}

func TestExportRendersMP4(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	s := testServer(t, fakeProvider{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	project, err := s.Projects.Create("Export")
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(project.Dir, "media", "clip.mp4")
	cmd := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "color=c=black:s=16x16:d=0.2", "-pix_fmt", "yuv420p", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed clip: %s %s", err, out)
	}
	body := `{"source":"media/clip.mp4","format":"mp4","quality":"draft","resolution":"source","audio":false,"filename":"out"}`
	resp, err := http.Post(ts.URL+"/v1/projects/"+project.ID+"/export", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("%s %s", resp.Status, raw)
	}
	var got struct {
		Media struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"media"`
		DownloadURL string `json:"download_url"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Media.Path != "exports/out.mp4" || got.DownloadURL == "" {
		t.Fatalf("export=%+v", got)
	}
}

func TestExportRequiresSource(t *testing.T) {
	s := testServer(t, fakeProvider{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	project, err := s.Projects.Create("Export")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(ts.URL+"/v1/projects/"+project.ID+"/export", "application/json", strings.NewReader(`{"format":"mp4"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestHealthAndSettings(t *testing.T) {
	s := testServer(t, fakeProvider{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}

	resp, err = http.Get(ts.URL + "/v1/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var pub config.Public
	if err := json.NewDecoder(resp.Body).Decode(&pub); err != nil {
		t.Fatal(err)
	}
	if !pub.APIKeySet || len(pub.Profiles) != 1 {
		t.Fatalf("expected seeded profile, got %+v", pub)
	}

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/settings", strings.NewReader(`{"base_url":"https://api.openai.com/v1","model":"gpt-4.1"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected put without active_id to fail, got %s", resp.Status)
	}
}

func TestSettingsSelectsEnvModel(t *testing.T) {
	s := testServer(t, fakeProvider{})
	s.Settings = config.NewStore(filepath.Join(t.TempDir(), "settings.json"), []config.LLM{
		{ID: "xai", Label: "Grok", BaseURL: "https://api.x.ai/v1", APIKey: "xai-secret", Model: "grok-4.6"},
		{ID: "openai", Label: "GPT", BaseURL: "https://api.openai.com/v1", APIKey: "sk-secret", Model: "gpt-4.1"},
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var pub config.Public
	if err := json.NewDecoder(resp.Body).Decode(&pub); err != nil {
		t.Fatal(err)
	}
	if pub.ActiveID != "xai" || len(pub.Profiles) != 2 {
		t.Fatalf("public=%+v", pub)
	}

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/settings", strings.NewReader(`{"active_id":"openai"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("select %s %s", resp.Status, raw)
	}
	if err := json.NewDecoder(resp.Body).Decode(&pub); err != nil {
		t.Fatal(err)
	}
	if pub.ActiveID != "openai" || pub.Model != "gpt-4.1" {
		t.Fatalf("selected=%+v", pub)
	}
	if s.Settings.Get().APIKey != "sk-secret" {
		t.Fatalf("active key=%q", s.Settings.Get().APIKey)
	}
}

func TestChatUsesProfileID(t *testing.T) {
	var used config.LLM
	s := testServer(t, fakeProvider{deltas: []llm.Delta{
		{Content: "ok", FinishReason: "stop"},
	}})
	s.Settings = config.NewStore(filepath.Join(t.TempDir(), "settings.json"), []config.LLM{
		{ID: "a", BaseURL: config.DefaultBaseURL, APIKey: "one", Model: "grok-4.6"},
		{ID: "b", BaseURL: "https://api.openai.com/v1", APIKey: "two", Model: "gpt-4.1"},
	})
	s.NewLLM = func(l config.LLM) llm.ChatProvider {
		used = l
		return fakeProvider{deltas: []llm.Delta{{Content: "ok", FinishReason: "stop"}}}
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	project, err := s.Projects.Create("Models")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"project_id":"` + project.ID + `","profile_id":"b","message":"hi"}`
	resp, err := http.Post(ts.URL+"/v1/agent/chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s", resp.Status, raw)
	}
	_, _ = io.ReadAll(resp.Body)
	if used.Model != "gpt-4.1" || used.APIKey != "two" {
		t.Fatalf("used=%+v", used)
	}
}

func TestChatSSE(t *testing.T) {
	s := testServer(t, fakeProvider{deltas: []llm.Delta{
		{Content: "Muted "},
		{Content: "the clip.", FinishReason: "stop"},
	}})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	project, err := s.Projects.Create("Chat project")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"project_id":"` + project.ID + `","message":"mute the clip"}`
	resp, err := http.Post(ts.URL+"/v1/agent/chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type %s", ct)
	}
	raw, _ := io.ReadAll(resp.Body)
	out := string(raw)
	if !strings.Contains(out, "event: session") {
		t.Fatalf("missing session: %s", out)
	}
	if !strings.Contains(out, "event: text") || !strings.Contains(out, "Muted") {
		t.Fatalf("missing text: %s", out)
	}
	if !strings.Contains(out, "event: done") {
		t.Fatalf("missing done: %s", out)
	}
}

func TestEmptyChatsListIsArray(t *testing.T) {
	s := testServer(t, fakeProvider{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	project, err := s.Projects.Create("Quiet")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(ts.URL + "/v1/projects/" + project.ID + "/chats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s", resp.Status, raw)
	}
	var listed struct {
		Chats []struct{} `json:"chats"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Chats == nil {
		t.Fatalf("chats should be [] not null: %s", raw)
	}
	if len(listed.Chats) != 0 {
		t.Fatalf("listed=%+v", listed)
	}
}

func TestProjectChatsPersist(t *testing.T) {
	s := testServer(t, fakeProvider{deltas: []llm.Delta{
		{Content: "Muted the clip.", FinishReason: "stop"},
	}})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	project, err := s.Projects.Create("Persisted")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(ts.URL+"/v1/projects/"+project.ID+"/chats", "application/json", strings.NewReader(`{"title":"Grade"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatal(resp.Status)
	}
	var created struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Title != "Grade" {
		t.Fatalf("title=%s", created.Title)
	}

	body := `{"project_id":"` + project.ID + `","session_id":"` + created.ID + `","message":"mute the clip"}`
	resp, err = http.Post(ts.URL+"/v1/agent/chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	_, _ = io.ReadAll(resp.Body)

	resp, err = http.Get(ts.URL + "/v1/projects/" + project.ID + "/chats/" + created.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Title    string `json:"title"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) < 2 {
		t.Fatalf("messages=%+v", got.Messages)
	}
	if got.Messages[0].Role != "user" || got.Messages[0].Content != "mute the clip" {
		t.Fatalf("first=%+v", got.Messages[0])
	}
	if !strings.Contains(got.Messages[len(got.Messages)-1].Content, "Muted") {
		t.Fatalf("assistant=%+v", got.Messages)
	}

	resp, err = http.Get(ts.URL + "/v1/projects/" + project.ID + "/chats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var listed struct {
		Chats []struct {
			ID string `json:"id"`
		} `json:"chats"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Chats) != 1 || listed.Chats[0].ID != created.ID {
		t.Fatalf("listed=%+v", listed)
	}
}

func TestProjectTimelineRoundTrip(t *testing.T) {
	s := testServer(t, fakeProvider{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	project, err := s.Projects.Create("Sequence")
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.URL + "/v1/projects/" + project.ID + "/timeline")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	var empty struct {
		Revision int `json:"revision"`
		FPS      int `json:"fps"`
		Clips    []struct {
			ID string `json:"id"`
		} `json:"clips"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&empty); err != nil {
		t.Fatal(err)
	}
	if empty.Revision != 0 || empty.FPS != 24 || len(empty.Clips) != 0 {
		t.Fatalf("empty=%+v", empty)
	}

	body := `{
		"schema":1,
		"fps":24,
		"playhead_frame":48,
		"selected_id":"clip-1",
		"px_per_second":28,
		"clips":[{
			"id":"clip-1",
			"name":"Highway",
			"track":"V1",
			"kind":"video",
			"start_frame":12,
			"duration_frames":72,
			"source_in_frame":8,
			"source_duration_frames":240,
			"media_path":"media/highway.mp4",
			"media_type":"video",
			"color":"#8a6a48"
		}]
	}`
	req, err := http.NewRequest(http.MethodPut, ts.URL+"/v1/projects/"+project.ID+"/timeline", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("put %s %s", resp.Status, raw)
	}
	var saved struct {
		Revision      int `json:"revision"`
		PlayheadFrame int `json:"playhead_frame"`
		Clips         []struct {
			ID            string `json:"id"`
			StartFrame    int    `json:"start_frame"`
			SourceInFrame int    `json:"source_in_frame"`
			MediaPath     string `json:"media_path"`
		} `json:"clips"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 1 || saved.PlayheadFrame != 48 || len(saved.Clips) != 1 {
		t.Fatalf("saved=%+v", saved)
	}
	if saved.Clips[0].SourceInFrame != 8 || saved.Clips[0].MediaPath != "media/highway.mp4" {
		t.Fatalf("clip=%+v", saved.Clips[0])
	}

	resp, err = http.Get(ts.URL + "/v1/projects/" + project.ID + "/timeline")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 1 || saved.Clips[0].StartFrame != 12 {
		t.Fatalf("reloaded=%+v", saved)
	}

	bad := `{"schema":1,"fps":24,"clips":[{"id":"x","track":"V1","kind":"video","duration_frames":10,"media_path":"../escape.mp4"}]}`
	req, err = http.NewRequest(http.MethodPut, ts.URL+"/v1/projects/"+project.ID+"/timeline", strings.NewReader(bad))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestEmptyHistorySerializesArrays(t *testing.T) {
	s := testServer(t, fakeProvider{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	project, err := s.Projects.Create("Empty history")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(ts.URL + "/v1/projects/" + project.ID + "/history")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Redo      json.RawMessage `json:"redo_candidates"`
		Revisions []struct {
			Children    json.RawMessage `json:"children"`
			Checkpoints json.RawMessage `json:"checkpoints"`
		} `json:"revisions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if string(body.Redo) != "[]" || len(body.Revisions) != 1 || string(body.Revisions[0].Children) != "[]" || string(body.Revisions[0].Checkpoints) != "[]" {
		t.Fatalf("history arrays: redo=%s revisions=%+v", body.Redo, body.Revisions)
	}
}

func TestTimelinePreflightAllowsRevisionHeaders(t *testing.T) {
	s := testServer(t, fakeProvider{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	req, err := http.NewRequest(http.MethodOptions, ts.URL+"/v1/projects/project/timeline", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", http.MethodPut)
	req.Header.Set("Access-Control-Request-Headers", "content-type,x-expected-revision,x-change-summary")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	headers := strings.ToLower(resp.Header.Get("Access-Control-Allow-Headers"))
	for _, required := range []string{"content-type", "x-expected-revision", "x-change-summary"} {
		if !strings.Contains(headers, required) {
			t.Fatalf("missing %s in %q", required, headers)
		}
	}
}

func TestChatRejectsUnknownProject(t *testing.T) {
	s := testServer(t, fakeProvider{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/agent/chat", "application/json", strings.NewReader(`{"project_id":"missing","message":"inspect"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestChatRequiresMessage(t *testing.T) {
	s := testServer(t, fakeProvider{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/agent/chat", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}
