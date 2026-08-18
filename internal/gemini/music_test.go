package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGenerateMusicInteractionsRequestAndResponse(t *testing.T) {
	audio := []byte("gemini-music")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/interactions" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if r.Header.Get("x-goog-api-key") != "secret" {
			t.Fatalf("missing Gemini API key")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "lyria-3-pro-preview" || body["input"] != "cinematic score" {
			t.Fatalf("body=%+v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "interaction-123",
			"steps": []any{map[string]any{
				"type": "model_output",
				"content": []any{
					map[string]any{"type": "text", "text": "[Verse]\nA bright new day"},
					map[string]any{"type": "audio", "mime_type": "audio/mpeg", "data": base64.StdEncoding.EncodeToString(audio)},
				},
			}},
		})
	}))
	defer server.Close()

	client := NewClient("secret", server.URL, time.Minute, 1<<20)
	got, err := client.GenerateMusic(context.Background(), MusicRequest{Prompt: "cinematic score", Model: "lyria-3-pro-preview"})
	if err != nil {
		t.Fatal(err)
	}
	if got.InteractionID != "interaction-123" || string(got.Audio) != string(audio) || !strings.Contains(got.Lyrics, "A bright new day") {
		t.Fatalf("result=%+v", got)
	}
}

func TestGenerateMusicWAVAddsResponseFormat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		format, ok := body["response_format"].(map[string]any)
		if !ok || format["type"] != "audio" {
			t.Fatalf("response_format=%+v", body["response_format"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"output_audio": map[string]any{"data": base64.StdEncoding.EncodeToString([]byte("wav")), "mime_type": "audio/wav"},
		})
	}))
	defer server.Close()

	client := NewClient("secret", server.URL, time.Minute, 1<<20)
	got, err := client.GenerateMusic(context.Background(), MusicRequest{Prompt: "ambient", Model: "lyria-3-pro-preview", OutputFormat: "wav"})
	if err != nil || string(got.Audio) != "wav" || got.MIMEType != "audio/wav" {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

func TestGenerateMusicRejectsWAVForClip(t *testing.T) {
	client := NewClient("secret", "http://127.0.0.1:1", time.Minute, 1<<20)
	if _, err := client.GenerateMusic(context.Background(), MusicRequest{Prompt: "clip", Model: "lyria-3-clip-preview", OutputFormat: "wav"}); err == nil {
		t.Fatal("expected clip WAV validation error")
	}
}
