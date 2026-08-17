package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"parallax/internal/agent"
	"parallax/internal/config"
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

func testServer(t *testing.T, p llm.ChatProvider) *Server {
	t.Helper()
	dir := t.TempDir()
	reg := tools.NewRegistry()
	projectStore, err := projects.NewStore(filepath.Join(dir, "projects"))
	if err != nil {
		t.Fatal(err)
	}
	return &Server{
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

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("files", "talk.mp4")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("video-bytes"))
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/projects/"+created.ID+"/media", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
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
				State string `json:"state"`
			} `json:"transcript"`
		} `json:"media"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Media) != 1 || listed.Media[0].Transcript == nil || listed.Media[0].Transcript.State != transcript.StateTranscribing {
		t.Fatalf("listed=%+v", listed)
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

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("files", "clip.mp4")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("video-bytes"))
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/projects/"+created.ID+"/media", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s", resp.Status, raw)
	}
	var upload struct {
		Media []struct {
			ContentURL string `json:"content_url"`
		} `json:"media"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&upload); err != nil {
		t.Fatal(err)
	}
	if len(upload.Media) != 1 {
		t.Fatalf("upload=%+v", upload)
	}
	resp, err = http.Get(ts.URL + upload.Media[0].ContentURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	served, _ := io.ReadAll(resp.Body)
	if string(served) != "video-bytes" {
		t.Fatalf("served=%q", served)
	}

	req, _ = http.NewRequest(http.MethodDelete, ts.URL+strings.Split(upload.Media[0].ContentURL, "?")[0], nil)
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
