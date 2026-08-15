package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMaskKey(t *testing.T) {
	if maskKey("") != "" {
		t.Fatal("empty key should mask to empty")
	}
	got := maskKey("xai-abcdefghij")
	if got == "xai-abcdefghij" {
		t.Fatal("key was not masked")
	}
	if !LooksMasked(got) {
		t.Fatalf("masked key %q not detected", got)
	}
	if LooksMasked("sk-live-real-secret") {
		t.Fatal("real key should not look masked")
	}
}

func TestStoreUpdateKeepsMaskedKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	s := NewStore(path, LLM{
		BaseURL: DefaultBaseURL,
		APIKey:  "secret-key-value",
		Model:   DefaultModel,
	})
	updated, err := s.Update(LLM{
		BaseURL: "https://api.openai.com/v1",
		APIKey:  "secr********alue",
		Model:   "gpt-4.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.APIKey != "secret-key-value" {
		t.Fatalf("api key overwritten: %q", updated.APIKey)
	}
	if updated.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("base url: %s", updated.BaseURL)
	}
	if updated.Model != "gpt-4.1" {
		t.Fatalf("model: %s", updated.Model)
	}

	// Reload from disk.
	s2 := NewStore(path, LLM{BaseURL: "https://x", APIKey: "y", Model: "z"})
	got := s2.Get()
	if got.APIKey != "secret-key-value" || got.Model != "gpt-4.1" {
		t.Fatalf("persisted settings not reloaded: %+v", got)
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
