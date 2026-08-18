package config_test

import (
	"os"
	"path/filepath"
	"testing"

	. "parallax/internal/config"
)

func TestValidateEmbedding(t *testing.T) {
	if err := ValidateEmbedding(Embedding{}); err == nil {
		t.Fatal("expected error")
	}
	if err := ValidateEmbedding(Embedding{BaseURL: "ftp://x", APIKey: "k", Model: "m"}); err == nil {
		t.Fatal("expected scheme error")
	}
	if err := ValidateEmbedding(Embedding{BaseURL: "https://api.openai.com/v1", APIKey: "k", Model: "text-embedding-3-small"}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateLLM(t *testing.T) {
	if err := ValidateLLM(LLM{}); err == nil {
		t.Fatal("expected error")
	}
	if err := ValidateLLM(LLM{BaseURL: "ftp://x", APIKey: "k", Model: "m"}); err == nil {
		t.Fatal("expected scheme error")
	}
	if err := ValidateLLM(LLM{BaseURL: DefaultBaseURL, APIKey: "k", Model: DefaultModel}); err != nil {
		t.Fatal(err)
	}
}

func TestStoreSelectsEnvProfiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	s := NewStore(path, []LLM{
		{ID: "grok", Label: "Grok", BaseURL: DefaultBaseURL, APIKey: "xai-secret", Model: DefaultModel},
		{ID: "gpt", Label: "GPT", BaseURL: "https://api.openai.com/v1", APIKey: "sk-secret", Model: "gpt-4.1"},
	})
	if s.Get().ID != "grok" {
		t.Fatalf("default active=%+v", s.Get())
	}
	selected, err := s.Select("gpt")
	if err != nil {
		t.Fatal(err)
	}
	if selected.Model != "gpt-4.1" || s.Get().APIKey != "sk-secret" {
		t.Fatalf("selected=%+v", selected)
	}

	s2 := NewStore(path, []LLM{
		{ID: "grok", BaseURL: DefaultBaseURL, APIKey: "xai-secret", Model: DefaultModel},
		{ID: "gpt", BaseURL: "https://api.openai.com/v1", APIKey: "sk-secret", Model: "gpt-4.1"},
	})
	if s2.Get().ID != "gpt" {
		t.Fatalf("persisted active=%+v", s2.Get())
	}
	if len(s2.Snapshot().Profiles) != 2 {
		t.Fatalf("profiles came from settings file: %+v", s2.Snapshot())
	}
}

func TestStoreIgnoresUnknownPersistedActive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"active_id":"missing"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(path, []LLM{
		{ID: "grok", BaseURL: DefaultBaseURL, APIKey: "k", Model: DefaultModel},
	})
	if s.Get().ID != "grok" {
		t.Fatalf("active=%+v", s.Get())
	}
}

func TestGetByID(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "settings.json"), []LLM{
		{ID: "a", BaseURL: DefaultBaseURL, APIKey: "one", Model: "grok-4.6"},
		{ID: "b", BaseURL: "https://api.openai.com/v1", APIKey: "two", Model: "gpt-4.1"},
	})
	got, err := s.GetByID("b")
	if err != nil || got.Model != "gpt-4.1" {
		t.Fatalf("get by id: %+v %v", got, err)
	}
	if _, err := s.GetByID("missing"); err == nil {
		t.Fatal("expected unknown id error")
	}
}

func TestResolveWhisperPathsFromRepoRoot(t *testing.T) {
	root := t.TempDir()
	backend := filepath.Join(root, "parallax_backend")
	python := filepath.Join(backend, "scripts", ".venv", "bin", "python")
	script := filepath.Join(backend, "scripts", "transcribe.py")
	if err := os.MkdirAll(filepath.Dir(python), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(python, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PARALLAX_WORKSPACE", filepath.Join(root, "ws"))
	t.Setenv("PARALLAX_DATA", filepath.Join(root, "data"))
	t.Setenv("WHISPER_PYTHON", "./scripts/.venv/bin/python")
	t.Setenv("WHISPER_SCRIPT", "./scripts/transcribe.py")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WhisperPython != python {
		t.Fatalf("python=%s want %s", cfg.WhisperPython, python)
	}
	if cfg.WhisperScript != script {
		t.Fatalf("script=%s want %s", cfg.WhisperScript, script)
	}
}

func TestLoadFFmpegHWAccel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PARALLAX_WORKSPACE", filepath.Join(dir, "ws"))
	t.Setenv("PARALLAX_DATA", filepath.Join(dir, "data"))
	t.Setenv("FFMPEG_HWACCEL", "off")
	t.Setenv("FFMPEG_HWDEVICE", "/dev/dri/renderD128")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FFmpegHWAccel != "off" {
		t.Fatalf("hwaccel=%q", cfg.FFmpegHWAccel)
	}
	if cfg.FFmpegHWDevice != "/dev/dri/renderD128" {
		t.Fatalf("hwdevice=%q", cfg.FFmpegHWDevice)
	}
}

func TestLoadEmbeddingQdrantAndWhisper(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PARALLAX_WORKSPACE", filepath.Join(dir, "ws"))
	t.Setenv("PARALLAX_DATA", filepath.Join(dir, "data"))
	t.Setenv("EMBEDDING_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("EMBEDDING_API_KEY", "sk-emb")
	t.Setenv("EMBEDDING_MODEL", "text-embedding-3-small")
	t.Setenv("QDRANT_URL", "http://127.0.0.1:6333")
	t.Setenv("WHISPER_MODEL", "/models/ggml-large-v3-turbo-q8_0.bin")
	t.Setenv("WHISPER_DEVICE", "cpu")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Embedding.Model != "text-embedding-3-small" || cfg.Embedding.APIKey != "sk-emb" {
		t.Fatalf("embed=%+v", cfg.Embedding)
	}
	if cfg.QdrantURL != "http://127.0.0.1:6333" {
		t.Fatalf("qdrant=%s", cfg.QdrantURL)
	}
	if cfg.WhisperModel != "/models/ggml-large-v3-turbo-q8_0.bin" || cfg.WhisperDevice != "cpu" {
		t.Fatalf("whisper=%s %s", cfg.WhisperModel, cfg.WhisperDevice)
	}
}

func TestLoadGeminiImageSettings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PARALLAX_WORKSPACE", filepath.Join(dir, "ws"))
	t.Setenv("PARALLAX_DATA", filepath.Join(dir, "data"))
	t.Setenv("GEMINI_API_KEY", "gemini-secret")
	t.Setenv("GEMINI_API_BASE", "https://generativelanguage.googleapis.com/v1beta/")
	t.Setenv("GEMINI_IMAGE_MODEL", "gemini-3.1-flash-image")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GeminiAPIKey != "gemini-secret" {
		t.Fatalf("key=%q", cfg.GeminiAPIKey)
	}
	if cfg.GeminiBaseURL != "https://generativelanguage.googleapis.com/v1beta" {
		t.Fatalf("base=%s", cfg.GeminiBaseURL)
	}
	if cfg.GeminiImageModel != "gemini-3.1-flash-image" {
		t.Fatalf("model=%s", cfg.GeminiImageModel)
	}
}

func TestLoadLLMProfilesFromModelsList(t *testing.T) {
	t.Setenv("LLM_PROFILES", "")
	t.Setenv("LLM_MODELS", "grok, gpt")
	t.Setenv("LLM_GROK_LABEL", "Grok")
	t.Setenv("LLM_GROK_BASE_URL", DefaultBaseURL)
	t.Setenv("LLM_GROK_MODEL", DefaultModel)
	t.Setenv("LLM_GROK_API_KEY", "xai-secret")
	t.Setenv("LLM_GPT_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("LLM_GPT_MODEL", "gpt-4.1")
	t.Setenv("LLM_GPT_API_KEY", "sk-secret")

	got := LoadLLMProfiles()
	if len(got) != 2 {
		t.Fatalf("profiles=%+v", got)
	}
	if got[0].ID != "grok" || got[0].Label != "Grok" || got[0].APIKey != "xai-secret" {
		t.Fatalf("first=%+v", got[0])
	}
	if got[1].ID != "gpt" || got[1].Model != "gpt-4.1" {
		t.Fatalf("second=%+v", got[1])
	}
}

func TestLoadLLMProfilesFromJSON(t *testing.T) {
	t.Setenv("LLM_MODELS", "")
	t.Setenv("LLM_PROFILES", `[{"id":"gemini","label":"Gemini","base_url":"https://generativelanguage.googleapis.com/v1beta/openai","model":"gemini-3.7-flash","api_key":"g-secret"}]`)
	got := LoadLLMProfiles()
	if len(got) != 1 || got[0].ID != "gemini" || got[0].APIKey != "g-secret" {
		t.Fatalf("profiles=%+v", got)
	}
}

func TestLoadLLMProfilesFallback(t *testing.T) {
	t.Setenv("LLM_MODELS", "")
	t.Setenv("LLM_PROFILES", "")
	t.Setenv("LLM_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("LLM_MODEL", "gpt-4.1")
	t.Setenv("LLM_API_KEY", "sk-fallback")
	got := LoadLLMProfiles()
	if len(got) != 1 || got[0].Model != "gpt-4.1" || got[0].APIKey != "sk-fallback" {
		t.Fatalf("fallback=%+v", got)
	}
}

func TestLoadDotEnvDoesNotOverride(t *testing.T) {
	t.Setenv("LLM_MODEL", "already-set")
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("LLM_MODEL=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	LoadDotEnv(path)
	if os.Getenv("LLM_MODEL") != "already-set" {
		t.Fatalf("env was overridden: %s", os.Getenv("LLM_MODEL"))
	}
}
