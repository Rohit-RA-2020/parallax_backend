package config

import (
	"os"
	"path/filepath"
	"testing"
)

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

	got := loadLLMProfiles()
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
	got := loadLLMProfiles()
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
	got := loadLLMProfiles()
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
	loadDotEnv(path)
	if os.Getenv("LLM_MODEL") != "already-set" {
		t.Fatalf("env was overridden: %s", os.Getenv("LLM_MODEL"))
	}
}
