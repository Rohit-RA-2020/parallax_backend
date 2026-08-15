// Package config loads process settings from the environment and an optional
// persisted settings file. LLM credentials are provider-agnostic: any
// OpenAI-compatible endpoint can be used by changing base URL, API key, and model.
package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const (
	DefaultAddr     = ":8080"
	DefaultBaseURL  = "https://api.x.ai/v1"
	DefaultModel    = "grok-4.6"
	DefaultMaxIters = 12
)

// LLM is the swap point for every model provider.
type LLM struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
}

// Config is the process-wide snapshot used at startup.
type Config struct {
	Addr         string
	WorkspaceDir string
	DataDir      string
	SettingsPath string
	MaxIters     int
	FFmpegBin    string
	FFprobeBin   string
	LLM          LLM
}

// Load reads optional .env files, then environment variables.
func Load() (Config, error) {
	loadDotEnv(".env")
	loadDotEnv(filepath.Join("..", ".env"))

	cwd, err := os.Getwd()
	if err != nil {
		return Config{}, err
	}

	workspace := envOr("PARALLAX_WORKSPACE", filepath.Join(cwd, "workspace"))
	data := envOr("PARALLAX_DATA", filepath.Join(cwd, "data"))

	workspace, err = filepath.Abs(workspace)
	if err != nil {
		return Config{}, fmt.Errorf("workspace: %w", err)
	}
	data, err = filepath.Abs(data)
	if err != nil {
		return Config{}, fmt.Errorf("data: %w", err)
	}

	cfg := Config{
		Addr:         envOr("PARALLAX_ADDR", DefaultAddr),
		WorkspaceDir: workspace,
		DataDir:      data,
		SettingsPath: filepath.Join(data, "settings.json"),
		MaxIters:     envInt("PARALLAX_MAX_ITERS", DefaultMaxIters),
		FFmpegBin:    envOr("FFMPEG_BIN", "ffmpeg"),
		FFprobeBin:   envOr("FFPROBE_BIN", "ffprobe"),
		LLM: LLM{
			BaseURL: envOr("LLM_BASE_URL", DefaultBaseURL),
			APIKey:  firstNonEmpty(os.Getenv("LLM_API_KEY"), os.Getenv("XAI_API_KEY")),
			Model:   envOr("LLM_MODEL", DefaultModel),
		},
	}

	if cfg.MaxIters < 1 {
		cfg.MaxIters = DefaultMaxIters
	}

	if err := os.MkdirAll(cfg.WorkspaceDir, 0o755); err != nil {
		return Config{}, fmt.Errorf("create workspace: %w", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return Config{}, fmt.Errorf("create data dir: %w", err)
	}

	return cfg, nil
}

// Public is the JSON shape returned to clients. The API key is never fully exposed.
type Public struct {
	BaseURL   string `json:"base_url"`
	Model     string `json:"model"`
	APIKeySet bool   `json:"api_key_set"`
	APIKey    string `json:"api_key"`
}

// Masked returns a client-safe view of the LLM settings.
func (l LLM) Masked() Public {
	return Public{
		BaseURL:   l.BaseURL,
		Model:     l.Model,
		APIKeySet: strings.TrimSpace(l.APIKey) != "",
		APIKey:    maskKey(l.APIKey),
	}
}

func maskKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return strings.Repeat("*", len(key))
	}
	return key[:4] + strings.Repeat("*", len(key)-8) + key[len(key)-4:]
}

// LooksMasked reports whether a submitted key is a placeholder, not a new secret.
func LooksMasked(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return true
	}
	return strings.Contains(key, "*")
}

// ValidateLLM checks that the three swap fields are usable.
func ValidateLLM(l LLM) error {
	if strings.TrimSpace(l.BaseURL) == "" {
		return errors.New("base_url is required")
	}
	if strings.TrimSpace(l.Model) == "" {
		return errors.New("model is required")
	}
	if strings.TrimSpace(l.APIKey) == "" {
		return errors.New("api_key is required")
	}
	if !strings.HasPrefix(l.BaseURL, "http://") && !strings.HasPrefix(l.BaseURL, "https://") {
		return errors.New("base_url must start with http:// or https://")
	}
	return nil
}

// Store holds the live LLM settings and persists them to disk.
type Store struct {
	mu   sync.RWMutex
	llm  LLM
	path string
}

func NewStore(path string, initial LLM) *Store {
	s := &Store{llm: initial, path: path}
	if loaded, err := readSettings(path); err == nil {
		s.llm = mergeLLM(initial, loaded)
	}
	return s
}

func (s *Store) Get() LLM {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.llm
}

func (s *Store) Update(next LLM) (LLM, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	merged := s.llm
	if strings.TrimSpace(next.BaseURL) != "" {
		merged.BaseURL = strings.TrimRight(strings.TrimSpace(next.BaseURL), "/")
	}
	if strings.TrimSpace(next.Model) != "" {
		merged.Model = strings.TrimSpace(next.Model)
	}
	if !LooksMasked(next.APIKey) {
		merged.APIKey = strings.TrimSpace(next.APIKey)
	}

	if err := ValidateLLM(merged); err != nil {
		return LLM{}, err
	}
	if err := writeSettings(s.path, merged); err != nil {
		return LLM{}, err
	}
	s.llm = merged
	return merged, nil
}

type persisted struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
}

func readSettings(path string) (LLM, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return LLM{}, err
	}
	var p persisted
	if err := json.Unmarshal(b, &p); err != nil {
		return LLM{}, err
	}
	return LLM{BaseURL: p.BaseURL, APIKey: p.APIKey, Model: p.Model}, nil
}

func writeSettings(path string, l LLM) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(persisted{
		BaseURL: l.BaseURL,
		APIKey:  l.APIKey,
		Model:   l.Model,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func mergeLLM(base, overlay LLM) LLM {
	out := base
	if strings.TrimSpace(overlay.BaseURL) != "" {
		out.BaseURL = overlay.BaseURL
	}
	if strings.TrimSpace(overlay.APIKey) != "" {
		out.APIKey = overlay.APIKey
	}
	if strings.TrimSpace(overlay.Model) != "" {
		out.Model = overlay.Model
	}
	return out
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// loadDotEnv is a tiny KEY=VALUE reader. Missing files are ignored.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		_ = os.Setenv(key, val)
	}
}
