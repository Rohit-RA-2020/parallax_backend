package llm_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	. "parallax/internal/llm"
)

func TestCompletionsURL(t *testing.T) {
	cases := map[string]string{
		"https://api.x.ai/v1":                  "https://api.x.ai/v1/chat/completions",
		"https://api.x.ai/v1/":                 "https://api.x.ai/v1/chat/completions",
		"https://api.x.ai/v1/chat/completions": "https://api.x.ai/v1/chat/completions",
		"":                                     "",
	}
	for in, want := range cases {
		if got := CompletionsURL(in); got != want {
			t.Errorf("CompletionsURL(%q)=%q want %q", in, got, want)
		}
	}
}

func TestAssembleToolCallsIncremental(t *testing.T) {
	deltas := []ToolCallDelta{
		{Index: 0, ID: "call_1", Type: "function", Function: struct {
			Name      string `json:"name,omitempty"`
			Arguments string `json:"arguments,omitempty"`
		}{Name: "run_ffmpeg", Arguments: `{"args":[`}},
		{Index: 0, Function: struct {
			Name      string `json:"name,omitempty"`
			Arguments string `json:"arguments,omitempty"`
		}{Arguments: `"-i","in.mp4"]}`}},
	}
	got := AssembleToolCalls(deltas)
	if len(got) != 1 {
		t.Fatalf("len=%d", len(got))
	}
	if got[0].Function.Name != "run_ffmpeg" {
		t.Fatalf("name=%s", got[0].Function.Name)
	}
	if !json.Valid([]byte(got[0].Function.Arguments)) {
		t.Fatalf("args not valid json: %s", got[0].Function.Arguments)
	}
}

func TestAssembleToolCallsWholeChunk(t *testing.T) {
	// xAI-style: the whole call arrives in one delta.
	got := AssembleToolCalls([]ToolCallDelta{{
		Index: 0,
		ID:    "call_9",
		Type:  "function",
		Function: struct {
			Name      string `json:"name,omitempty"`
			Arguments string `json:"arguments,omitempty"`
		}{Name: "probe_media", Arguments: `{"path":"a.mp4"}`},
	}})
	if len(got) != 1 || got[0].Function.Name != "probe_media" {
		t.Fatalf("%+v", got)
	}
}

func TestAssembleToolCallsDeduplicatesRepeatedCompleteArguments(t *testing.T) {
	part := ToolCallDelta{Index: 0, ID: "call_10", Type: "function", Function: struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	}{Name: "get_timeline", Arguments: `{}`}}
	got := AssembleToolCalls([]ToolCallDelta{part, part})
	if len(got) != 1 {
		t.Fatalf("len=%d", len(got))
	}
	if got[0].Function.Arguments != `{}` {
		t.Fatalf("arguments=%q", got[0].Function.Arguments)
	}
}

func TestAssembleToolCallsSplitsParallelCallsThatReuseIndex(t *testing.T) {
	got := AssembleToolCalls([]ToolCallDelta{
		{Index: 0, ID: "call_1", Type: "function", Function: struct {
			Name      string `json:"name,omitempty"`
			Arguments string `json:"arguments,omitempty"`
		}{Name: "probe_media", Arguments: `{"path":"media/sample-5s.mp4"}`}},
		{Index: 0, ID: "call_2", Type: "function", Function: struct {
			Name      string `json:"name,omitempty"`
			Arguments string `json:"arguments,omitempty"`
		}{Name: "get_timeline", Arguments: `{}`}},
	})
	if len(got) != 2 {
		t.Fatalf("len=%d got=%+v", len(got), got)
	}
	if got[0].Function.Name != "probe_media" || got[0].Function.Arguments != `{"path":"media/sample-5s.mp4"}` {
		t.Fatalf("first=%+v", got[0])
	}
	if got[1].Function.Name != "get_timeline" || got[1].Function.Arguments != `{}` {
		t.Fatalf("second=%+v", got[1])
	}
}

func TestAssembleToolCallsMergesConcatenatedGetTimelineArguments(t *testing.T) {
	// Exact Director failure: Gemini reused index 0 and appended {} after a
	// probe-shaped object, producing invalid JSON for get_timeline.
	got := AssembleToolCalls([]ToolCallDelta{
		{Index: 0, ID: "call_1793426", Type: "function", Function: struct {
			Name      string `json:"name,omitempty"`
			Arguments string `json:"arguments,omitempty"`
		}{Name: "get_timeline", Arguments: `{"path":"media/sample-5s.mp4"}`}},
		{Index: 0, Function: struct {
			Name      string `json:"name,omitempty"`
			Arguments string `json:"arguments,omitempty"`
		}{Arguments: `{}`}},
	})
	if len(got) != 1 {
		t.Fatalf("len=%d got=%+v", len(got), got)
	}
	if !json.Valid([]byte(got[0].Function.Arguments)) {
		t.Fatalf("arguments are not valid JSON: %s", got[0].Function.Arguments)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(got[0].Function.Arguments), &body); err != nil {
		t.Fatal(err)
	}
	if body["path"] != "media/sample-5s.mp4" {
		t.Fatalf("arguments=%s", got[0].Function.Arguments)
	}
}

func TestAssembleToolCallsRepairsSingleChunkConcatenatedArguments(t *testing.T) {
	got := AssembleToolCalls([]ToolCallDelta{{
		Index: 0,
		ID:    "call_1793426",
		Type:  "function",
		Function: struct {
			Name      string `json:"name,omitempty"`
			Arguments string `json:"arguments,omitempty"`
		}{Name: "get_timeline", Arguments: `{"path":"media/sample-5s.mp4"}{}`},
	}})
	if len(got) != 1 {
		t.Fatalf("len=%d", len(got))
	}
	if !json.Valid([]byte(got[0].Function.Arguments)) {
		t.Fatalf("arguments are not valid JSON: %s", got[0].Function.Arguments)
	}
	if got[0].Function.Name != "get_timeline" {
		t.Fatalf("name=%s", got[0].Function.Name)
	}
}

func TestCompatClientSanitizesConcatenatedArgumentsFromStoredSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		got := request.Messages[0].ToolCalls[0].Function.Arguments
		if !json.Valid([]byte(got)) {
			t.Errorf("arguments=%q", got)
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(got), &body); err != nil {
			t.Fatal(err)
		}
		if body["path"] != "media/sample-5s.mp4" {
			t.Errorf("arguments=%q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := NewCompatClient(srv.URL+"/v1", "test-key", "gemini-test")
	c.HTTPClient = srv.Client()
	stream, err := c.Stream(context.Background(), Request{
		Messages: []Message{
			{
				Role: RoleAssistant,
				ToolCalls: []ToolCall{
					{ID: "call_1793426", Type: "function", Function: FunctionCall{
						Name: "get_timeline", Arguments: `{"path":"media/sample-5s.mp4"}{}`,
					}},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for delta := range stream {
		if delta.Err != nil {
			t.Fatal(delta.Err)
		}
	}
}

func TestCompatClientSanitizesRepeatedArgumentsFromStoredSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		got := request.Messages[0].ToolCalls[0].Function.Arguments
		if got != `{}` {
			t.Errorf("arguments=%q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := NewCompatClient(srv.URL+"/v1", "test-key", "gemini-test")
	c.HTTPClient = srv.Client()
	stream, err := c.Stream(context.Background(), Request{
		Messages: []Message{
			{
				Role: RoleAssistant,
				ToolCalls: []ToolCall{
					{ID: "call_10", Type: "function", Function: FunctionCall{
						Name: "get_timeline", Arguments: `{}{} `,
					}},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for delta := range stream {
		if delta.Err != nil {
			t.Fatal(delta.Err)
		}
	}
}

func TestAssembleToolCallsPreservesGeminiThoughtSignature(t *testing.T) {
	extra := json.RawMessage(`{"google":{"thought_signature":"encrypted-signature"}}`)
	got := AssembleToolCalls([]ToolCallDelta{{
		Index:        0,
		ID:           "function-call-1",
		Type:         "function",
		ExtraContent: extra,
		Function: struct {
			Name      string `json:"name,omitempty"`
			Arguments string `json:"arguments,omitempty"`
		}{Name: "list_workspace", Arguments: `{}`},
	}})
	if len(got) != 1 {
		t.Fatalf("len=%d", len(got))
	}
	if string(got[0].ExtraContent) != string(extra) {
		t.Fatalf("extra_content=%s", got[0].ExtraContent)
	}
	b, err := json.Marshal(Message{Role: RoleAssistant, ToolCalls: got})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"thought_signature":"encrypted-signature"`) {
		t.Fatalf("signature missing from replayed assistant message: %s", b)
	}
}

func TestCompatClientStreamsGeminiThoughtSignature(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"function-call-1","type":"function","extra_content":{"google":{"thought_signature":"sig-123"}},"function":{"name":"list_workspace","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := NewCompatClient(srv.URL+"/v1", "test-key", "gemini-3-flash-preview")
	c.HTTPClient = srv.Client()
	stream, err := c.Stream(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "list files"}}})
	if err != nil {
		t.Fatal(err)
	}
	var parts []ToolCallDelta
	for delta := range stream {
		if delta.Err != nil {
			t.Fatal(delta.Err)
		}
		parts = append(parts, delta.ToolCalls...)
	}
	calls := AssembleToolCalls(parts)
	if len(calls) != 1 || string(calls[0].ExtraContent) != `{"google":{"thought_signature":"sig-123"}}` {
		t.Fatalf("calls=%+v", calls)
	}
}

func TestCompatClientComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		var body struct {
			Stream bool   `json:"stream"`
			Model  string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Stream || body.Model != "grok-4.6" {
			t.Fatalf("body=%+v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": `["Thanks"]`}}},
		})
	}))
	defer srv.Close()

	c := NewCompatClient(srv.URL+"/v1", "test-key", "grok-4.6")
	c.HTTPClient = srv.Client()
	got, err := c.Complete(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "translate"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != `["Thanks"]` {
		t.Fatalf("got=%q", got)
	}
}

func TestCompatClientStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("auth %s", r.Header.Get("Authorization"))
		}
		var body struct {
			ReasoningEffort string `json:"reasoning_effort"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		} else if body.ReasoningEffort != "high" {
			t.Errorf("reasoning_effort=%q", body.ReasoningEffort)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := NewCompatClient(srv.URL+"/v1", "test-key", "grok-4.6")
	c.HTTPClient = srv.Client()
	ch, err := c.Stream(context.Background(), Request{
		Messages:        []Message{{Role: RoleUser, Content: "hi"}},
		ReasoningEffort: ThinkingEffortHigh,
	})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var reason string
	for d := range ch {
		if d.Err != nil {
			t.Fatal(d.Err)
		}
		text.WriteString(d.Content)
		if d.FinishReason != "" {
			reason = d.FinishReason
		}
	}
	if text.String() != "Hello" {
		t.Fatalf("text=%q", text.String())
	}
	if reason != "stop" {
		t.Fatalf("reason=%q", reason)
	}
}

func TestCompatClientHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()

	c := NewCompatClient(srv.URL+"/v1", "x", "grok-4.6")
	c.HTTPClient = srv.Client()
	_, err := c.Stream(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("err=%v", err)
	}
}
