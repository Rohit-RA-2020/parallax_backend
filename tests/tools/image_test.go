package tools_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "parallax/internal/tools"
)

func decodeTinyJPEG(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(tinyJPEG)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

const tinyJPEG = "/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAIBAQEBAQIBAQECAgICAgQDAgICAgUEBAMEBgUGBgYFBgYGBwkIBgcJBwYGCAsICQoKCgoKBggLDAsKDAkKCgr/2wBDAQICAgICAgUDAwUKBwYHCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgr/wAARCAABAAEDASIAAhEBAxEB/8QAHwAAAQUBAQEBAQEAAAAAAAAAAAECAwQFBgcICQoL/8QAtRAAAgEDAwIEAwUFBAQAAAF9AQIDAAQRBRIhMUEGE1FhByJxFDKBkaEII0KxwRVS0fAkM2JyggkKFhcYGRolJicoKSo0NTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqDhIWGh4iJipKTlJWWl5iZmqKjpKWmp6ipqrKztLW2t7i5usLDxMXGx8jJytLT1NXW19jZ2uHi4+Tl5ufo6erx8vP09fb3+Pn6/8QAHwEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoL/8QAtREAAgECBAQDBAcFBAQAAQJ3AAECAxEEBSExBhJBUQdhcRMiMoEIFEKRobHBCSMzUvAVYnLRChYkNOEl8RcYGRomJygpKjU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6goOEhYaHiImKkpOUlZaXmJmaoqOkpaanqKmqsrO0tba3uLm6wsPExcbHyMnK0tPU1dbX2Nna4uPk5ebn6Onq8vP09fb3+Pn6/9oADAMBAAIRAxEAPwD4vooor+Uz/fw//9k="

func TestGenerateImageWritesBinFile(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/interactions" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		gotAuth = r.Header.Get("x-goog-api-key")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output_image":{"type":"image","mime_type":"image/jpeg","data":"` + tinyJPEG + `"}}`))
	}))
	defer server.Close()

	ws := t.TempDir()
	var mutated bool
	var appliedRel, appliedPrompt string
	reg := NewRegistry()
	RegisterImage(reg, ImageEnv{
		Workspace:  ws,
		APIKey:     "gemini-test-key",
		BaseURL:    server.URL + "/v1beta",
		Model:      "gemini-3.1-flash-image",
		OnMutation: func() { mutated = true },
		OnApplied: func(rel, prompt string) {
			appliedRel = rel
			appliedPrompt = prompt
		},
	})

	res := reg.Execute(context.Background(), "generate_image", `{
		"prompt":"A cinematic wide shot of a rain-soaked neon alley at night, wet asphalt reflections, 35mm anamorphic",
		"aspect_ratio":"16:9",
		"filename":"neon alley.png"
	}`)
	if !res.OK {
		t.Fatal(res.Error)
	}
	if gotAuth != "gemini-test-key" {
		t.Fatalf("auth=%q", gotAuth)
	}
	if gotBody["model"] != "gemini-3.1-flash-image" || gotBody["store"] != false {
		t.Fatalf("body=%#v", gotBody)
	}
	format := gotBody["response_format"].(map[string]any)
	if format["mime_type"] != "image/jpeg" || format["aspect_ratio"] != "16:9" || format["image_size"] != "1K" {
		t.Fatalf("format=%#v", format)
	}

	out := res.Output.(map[string]any)
	if out["path"] != "media/neon-alley.jpg" {
		t.Fatalf("path=%v", out["path"])
	}
	if out["width"] != 1 || out["height"] != 1 {
		t.Fatalf("size=%v x %v", out["width"], out["height"])
	}
	if !mutated {
		t.Fatal("expected media mutation")
	}
	if appliedRel != "media/neon-alley.jpg" || !strings.Contains(appliedPrompt, "neon alley") {
		t.Fatalf("applied rel=%q prompt=%q", appliedRel, appliedPrompt)
	}
	data, err := os.ReadFile(filepath.Join(ws, "media", "neon-alley.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 3 || data[0] != 0xff || data[1] != 0xd8 {
		t.Fatalf("saved file is not a JPEG: %d bytes", len(data))
	}
}

func TestGenerateImageReadsModelOutputWhenConvenienceFieldMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"steps":[
				{"type":"thought","content":[{"type":"image","mime_type":"image/png","data":"AAAA"}]},
				{"type":"model_output","content":[{"type":"text","text":"Here is the still."},{"type":"image","mime_type":"image/jpeg","data":"` + tinyJPEG + `"}]}
			]
		}`))
	}))
	defer server.Close()

	ws := t.TempDir()
	reg := NewRegistry()
	RegisterImage(reg, ImageEnv{Workspace: ws, APIKey: "k", BaseURL: server.URL})
	res := reg.Execute(context.Background(), "generate_image", `{"prompt":"title card for a night drive","filename":"title-card"}`)
	if !res.OK {
		t.Fatal(res.Error)
	}
	out := res.Output.(map[string]any)
	if out["path"] != "media/title-card.jpg" {
		t.Fatalf("path=%v", out["path"])
	}
	if out["model_text"] != "Here is the still." {
		t.Fatalf("text=%v", out["model_text"])
	}
}

func TestGenerateImageRequiresAPIKeyAndPrompt(t *testing.T) {
	reg := NewRegistry()
	RegisterImage(reg, ImageEnv{Workspace: t.TempDir()})
	res := reg.Execute(context.Background(), "generate_image", `{"prompt":"anything"}`)
	if res.OK || !strings.Contains(res.Error, "GEMINI_API_KEY") {
		t.Fatalf("missing key: %+v", res)
	}

	RegisterImage(reg, ImageEnv{Workspace: t.TempDir(), APIKey: "k", BaseURL: "http://127.0.0.1:1"})
	res = reg.Execute(context.Background(), "generate_image", `{"prompt":"   "}`)
	if res.OK || !strings.Contains(res.Error, "prompt") {
		t.Fatalf("empty prompt: %+v", res)
	}

	res = reg.Execute(context.Background(), "generate_image", `{"prompt":"ok","aspect_ratio":"7:3"}`)
	if res.OK || !strings.Contains(res.Error, "aspect_ratio") {
		t.Fatalf("bad aspect: %+v", res)
	}
}

func TestGenerateImageAvoidsFilenameCollision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output_image":{"data":"` + tinyJPEG + `","mime_type":"image/jpeg"}}`))
	}))
	defer server.Close()

	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "media", "sunset.jpg"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry()
	RegisterImage(reg, ImageEnv{Workspace: ws, APIKey: "k", BaseURL: server.URL})
	res := reg.Execute(context.Background(), "generate_image", `{"prompt":"golden hour over a desert highway","filename":"sunset.png"}`)
	if !res.OK {
		t.Fatal(res.Error)
	}
	out := res.Output.(map[string]any)
	if out["path"] != "media/sunset-1.jpg" {
		t.Fatalf("path=%v", out["path"])
	}
}

func TestGenerateImageReportsGeminiErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"prompt blocked"}}`))
	}))
	defer server.Close()

	reg := NewRegistry()
	RegisterImage(reg, ImageEnv{Workspace: t.TempDir(), APIKey: "k", BaseURL: server.URL})
	res := reg.Execute(context.Background(), "generate_image", `{"prompt":"blocked subject"}`)
	if res.OK || !strings.Contains(res.Error, "HTTP 400") {
		t.Fatalf("result=%+v", res)
	}
}

func TestGenerateImageEditsSourceInPlace(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output_image":{"mime_type":"image/jpeg","data":"` + tinyJPEG + `"}}`))
	}))
	defer server.Close()

	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := decodeTinyJPEG(t)
	if err := os.WriteFile(filepath.Join(ws, "media", "neon-alley.jpg"), src, 0o644); err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry()
	RegisterImage(reg, ImageEnv{Workspace: ws, APIKey: "k", BaseURL: server.URL})
	res := reg.Execute(context.Background(), "generate_image", `{
		"prompt":"Keep the same alley. Add heavier rain and brighter magenta neon in the puddles.",
		"source":"media/neon-alley.jpg"
	}`)
	if !res.OK {
		t.Fatal(res.Error)
	}
	input, _ := gotBody["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("input=%#v", gotBody["input"])
	}
	text := input[0].(map[string]any)
	image := input[1].(map[string]any)
	if text["type"] != "text" || !strings.Contains(text["text"].(string), "heavier rain") {
		t.Fatalf("text=%#v", text)
	}
	if image["type"] != "image" || image["mime_type"] != "image/jpeg" || image["data"] == "" {
		t.Fatalf("image=%#v", image)
	}
	out := res.Output.(map[string]any)
	if out["path"] != "media/neon-alley.jpg" || out["applied_to"] != "media/neon-alley.jpg" || out["edited"] != true {
		t.Fatalf("output=%#v", out)
	}
	if _, err := os.Stat(filepath.Join(ws, "media", "neon-alley-edit.jpg")); !os.IsNotExist(err) {
		t.Fatal("in-place edit left a sibling copy")
	}
}

func TestGenerateImageKeepsVariantWhenApplyNone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output_image":{"mime_type":"image/jpeg","data":"` + tinyJPEG + `"}}`))
	}))
	defer server.Close()

	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "media", "neon-alley.jpg"), decodeTinyJPEG(t), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry()
	RegisterImage(reg, ImageEnv{Workspace: ws, APIKey: "k", BaseURL: server.URL})
	res := reg.Execute(context.Background(), "generate_image", `{
		"prompt":"Same alley, daylight version",
		"source":"media/neon-alley.jpg",
		"apply_to":"none"
	}`)
	if !res.OK {
		t.Fatal(res.Error)
	}
	out := res.Output.(map[string]any)
	if out["path"] != "media/neon-alley-edit.jpg" || out["in_place"] != false {
		t.Fatalf("output=%#v", out)
	}
	if _, err := os.Stat(filepath.Join(ws, "media", "neon-alley.jpg")); err != nil {
		t.Fatal("original was removed")
	}
}

func TestGenerateImageSendsMultipleReferences(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output_image":{"mime_type":"image/jpeg","data":"` + tinyJPEG + `"}}`))
	}))
	defer server.Close()

	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	jpeg := decodeTinyJPEG(t)
	if err := os.WriteFile(filepath.Join(ws, "media", "subject.jpg"), jpeg, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "media", "logo.jpg"), jpeg, 0o644); err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry()
	RegisterImage(reg, ImageEnv{Workspace: ws, APIKey: "k", BaseURL: server.URL})
	res := reg.Execute(context.Background(), "generate_image", `{
		"prompt":"Put the logo on the subject's jacket",
		"source":"media/subject.jpg",
		"images":["media/logo.jpg"]
	}`)
	if !res.OK {
		t.Fatal(res.Error)
	}
	input, _ := gotBody["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input=%#v", gotBody["input"])
	}
	if input[1].(map[string]any)["type"] != "image" || input[2].(map[string]any)["type"] != "image" {
		t.Fatalf("input=%#v", input)
	}
	out := res.Output.(map[string]any)
	if out["applied_to"] != "media/subject.jpg" {
		t.Fatalf("output=%#v", out)
	}
}

func TestGenerateImageRejectsBadSource(t *testing.T) {
	reg := NewRegistry()
	RegisterImage(reg, ImageEnv{Workspace: t.TempDir(), APIKey: "k", BaseURL: "http://127.0.0.1:1"})
	missing := reg.Execute(context.Background(), "generate_image", `{"prompt":"edit this","source":"media/missing.jpg"}`)
	if missing.OK || !strings.Contains(missing.Error, "not found") {
		t.Fatalf("missing=%+v", missing)
	}

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	RegisterImage(reg, ImageEnv{Workspace: ws, APIKey: "k", BaseURL: "http://127.0.0.1:1"})
	notImage := reg.Execute(context.Background(), "generate_image", `{"prompt":"edit this","source":"notes.txt"}`)
	if notImage.OK || !strings.Contains(notImage.Error, "image") {
		t.Fatalf("not image=%+v", notImage)
	}

	escaped := reg.Execute(context.Background(), "generate_image", `{"prompt":"edit this","source":"../secret.jpg"}`)
	if escaped.OK {
		t.Fatal("path escape succeeded")
	}
}

func TestGenerateImageRejectsTextOnlyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output_text":"I cannot create that image."}`))
	}))
	defer server.Close()

	reg := NewRegistry()
	RegisterImage(reg, ImageEnv{Workspace: t.TempDir(), APIKey: "k", BaseURL: server.URL})
	res := reg.Execute(context.Background(), "generate_image", `{"prompt":"something unsafe"}`)
	if res.OK || !strings.Contains(res.Error, "I cannot create that image") {
		t.Fatalf("result=%+v", res)
	}
}
