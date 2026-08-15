package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"parallax/internal/agent"
	"parallax/internal/config"
	"parallax/internal/llm"
	"parallax/internal/tools"
)

type fakeProvider struct {
	deltas []llm.Delta
}

func (f fakeProvider) Stream(_ context.Context, _ llm.Request) (<-chan llm.Delta, error) {
	ch := make(chan llm.Delta, len(f.deltas))
	for _, d := range f.deltas {
		ch <- d
	}
	close(ch)
	return ch, nil
}

func testServer(t *testing.T, p llm.ChatProvider) *Server {
	t.Helper()
	dir := t.TempDir()
	reg := tools.NewRegistry()
	return &Server{
		Settings: config.NewStore(filepath.Join(dir, "settings.json"), config.LLM{
			BaseURL: config.DefaultBaseURL,
			APIKey:  "test-key",
			Model:   config.DefaultModel,
		}),
		Sessions:  agent.NewStore(),
		Tools:     reg,
		MaxIters:  4,
		Workspace: dir,
		NewLLM:    func(config.LLM) llm.ChatProvider { return p },
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
	if !pub.APIKeySet || strings.Contains(pub.APIKey, "test-key") {
		t.Fatalf("key leak or unset: %+v", pub)
	}

	body, _ := json.Marshal(config.LLM{
		BaseURL: "https://api.openai.com/v1",
		Model:   "gpt-4.1",
		APIKey:  pub.APIKey, // masked — should keep existing
	})
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/settings", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s", resp.Status, b)
	}
	if s.Settings.Get().APIKey != "test-key" {
		t.Fatal("masked put overwrote key")
	}
	if s.Settings.Get().Model != "gpt-4.1" {
		t.Fatalf("model=%s", s.Settings.Get().Model)
	}
}

func TestChatSSE(t *testing.T) {
	s := testServer(t, fakeProvider{deltas: []llm.Delta{
		{Content: "Muted "},
		{Content: "the clip.", FinishReason: "stop"},
	}})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	body := `{"message":"mute the clip"}`
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
