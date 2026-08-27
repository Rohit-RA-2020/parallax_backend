package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"parallax/internal/gemini"
)

func TestChooseVideoProvider(t *testing.T) {
	tests := []struct {
		name     string
		task     string
		previous string
		video    bool
		last     bool
		refs     int
		duration int
		res      string
		want     string
	}{
		{name: "default omni", task: "text_to_video", want: "omni"},
		{name: "omni continuation", task: "edit", previous: "interaction-1", want: "omni"},
		{name: "veo reference", task: "reference_to_video", refs: 1, want: "veo"},
		{name: "veo interpolation", task: "interpolate", last: true, want: "veo"},
		{name: "veo extension", task: "extend", video: true, want: "veo"},
		{name: "veo duration", task: "text_to_video", duration: 6, want: "veo"},
		{name: "veo resolution", task: "text_to_video", res: "4k", want: "veo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := chooseVideoProvider(tt.task, tt.previous, tt.video, tt.last, tt.refs, tt.duration, tt.res)
			if err != nil || got != tt.want {
				t.Fatalf("provider=%q err=%v want=%q", got, err, tt.want)
			}
		})
	}
}

func TestChooseVideoProviderRejectsConflictingContinuation(t *testing.T) {
	if _, err := chooseVideoProvider("edit", "interaction-1", false, false, 0, 8, ""); err == nil {
		t.Fatal("expected Omni/Veo conflict")
	}
}

func TestChooseVideoProviderValidatesSpecialTasks(t *testing.T) {
	if _, err := chooseVideoProvider("extend", "", false, false, 0, 0, ""); err == nil {
		t.Fatal("expected missing source video error")
	}
	if _, err := chooseVideoProvider("interpolate", "", false, false, 0, 0, ""); err == nil {
		t.Fatal("expected missing last frame error")
	}
}

func TestGenerateVideoUsesDefaultChatAttachmentBytes(t *testing.T) {
	rawImage := []byte{0x89, 0x50, 0x4e, 0x47, 0x00, 0xff}
	rawVideo := []byte("video-bytes")
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/interactions" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "interaction-1",
			"steps": []any{map[string]any{
				"content": []any{map[string]any{
					"type": "video", "mime_type": "video/mp4",
					"data": base64.StdEncoding.EncodeToString(rawVideo),
				}},
			}},
		})
	}))
	defer server.Close()

	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "media", "chat-reference.png"), rawImage, 0o644); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	RegisterVideoGeneration(reg, VideoGenerationEnv{
		Workspace:     workspace,
		Client:        gemini.NewClient("k", server.URL, time.Second, 1<<20),
		DefaultImages: []string{"media/chat-reference.png"},
	})
	res := reg.Execute(context.Background(), "generate_video", `{"prompt":"animate this exact image with a slow camera move"}`)
	if !res.OK {
		t.Fatal(res.Error)
	}
	input := gotBody["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("input=%#v", gotBody["input"])
	}
	imagePart := input[0].(map[string]any)
	got, err := base64.StdEncoding.DecodeString(imagePart["data"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, rawImage) {
		t.Fatal("default chat attachment bytes were changed before video generation")
	}
}
