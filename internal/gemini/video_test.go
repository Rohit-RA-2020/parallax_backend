package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGenerateOmniInlineVideo(t *testing.T) {
	want := []byte("omni-video")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/interactions" || r.Method != http.MethodPost {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("x-goog-api-key") != "secret" {
			t.Fatal("missing API key")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != DefaultOmniVideoModel || body["input"] != "a cinematic shot" {
			t.Fatalf("body=%+v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "interaction-1",
			"steps": []any{map[string]any{
				"content": []any{map[string]any{
					"type": "video", "mime_type": "video/mp4",
					"data": base64.StdEncoding.EncodeToString(want),
				}},
			}},
		})
	}))
	defer server.Close()

	client := NewClient("secret", server.URL, time.Second, 1<<20)
	got, err := client.GenerateOmni(context.Background(), VideoRequest{Prompt: "a cinematic shot"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "omni" || got.InteractionID != "interaction-1" || string(got.Video) != string(want) {
		t.Fatalf("result=%+v", got)
	}
}

func TestGenerateVeoPollsAndDownloadsVideo(t *testing.T) {
	want := []byte("veo-video")
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/models/veo-3.1-generate-preview:predictLongRunning":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			parameters := body["parameters"].(map[string]any)
			if parameters["durationSeconds"] != "8" || parameters["resolution"] != "720p" {
				t.Fatalf("parameters=%+v", parameters)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "operations/op-1"})
		case r.Method == http.MethodGet && r.URL.Path == "/operations/op-1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"done": true,
				"response": map[string]any{
					"generateVideoResponse": map[string]any{
						"generatedSamples": []any{map[string]any{
							"video": map[string]any{"uri": server.URL + "/download/video"},
						}},
					},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/download/video":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(want)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient("secret", server.URL, time.Second, 1<<20)
	got, err := client.GenerateVeo(context.Background(), VideoRequest{Prompt: "a cinematic shot"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "veo" || string(got.Video) != string(want) || got.VideoURI == "" {
		t.Fatalf("result=%+v", got)
	}
}
