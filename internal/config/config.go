// Package config loads process settings from the environment and an optional
// persisted settings file. LLM credentials are provider-agnostic: any
// OpenAI-compatible endpoint can be used by changing base URL, API key, and model.
// Multiple models are declared in the environment; the UI only selects among them.
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
	DefaultAddr      = ":8080"
	DefaultBaseURL   = "https://api.x.ai/v1"
	DefaultModel     = "grok-4.6"
	DefaultMaxIters  = 50
	defaultProfileID = "default"
)

// LLM is one OpenAI-compatible model endpoint.
type LLM struct {
	ID      string `json:"id"`
	Label   string `json:"label,omitempty"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
}

// Settings is the in-memory multi-model snapshot.
type Settings struct {
	ActiveID string
	Profiles []LLM
}

// Config is the process-wide snapshot used at startup.
type Config struct {
	Addr             string
	WorkspaceDir     string
	DataDir          string
	SettingsPath     string
	ExaAPIKey        string
	ExaBaseURL       string
	GeminiAPIKey     string
	GeminiBaseURL    string
	GeminiImageModel string
	MaxIters         int
	FFmpegBin        string
	FFprobeBin       string
	FFmpegHWAccel    string
	FFmpegHWDevice   string
	LLMs             []LLM
	Embedding        Embedding
	QdrantURL        string
	QdrantAPIKey     string
	WhisperModel     string
	WhisperDevice    string
	WhisperPython    string
	WhisperScript    string
	WhisperCompute   string
}

// Embedding is a separate OpenAI-compatible /v1/embeddings endpoint.
// It must not reuse LLM_* credentials.
type Embedding struct {
	BaseURL string
	APIKey  string
	Model   string
}

// Load reads optional .env files, then environment variables.
func Load() (Config, error) {
	LoadDotEnv(".env")
	LoadDotEnv(filepath.Join("..", ".env"))

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
		Addr:             envOr("PARALLAX_ADDR", DefaultAddr),
		WorkspaceDir:     workspace,
		DataDir:          data,
		SettingsPath:     filepath.Join(data, "settings.json"),
		ExaAPIKey:        strings.TrimSpace(os.Getenv("EXA_API_KEY")),
		ExaBaseURL:       envOr("EXA_BASE_URL", "https://api.exa.ai"),
		GeminiAPIKey:     firstNonEmpty(os.Getenv("GEMINI_API_KEY"), os.Getenv("GOOGLE_API_KEY")),
		GeminiBaseURL:    strings.TrimRight(envOr("GEMINI_API_BASE", "https://generativelanguage.googleapis.com/v1beta"), "/"),
		GeminiImageModel: envOr("GEMINI_IMAGE_MODEL", "gemini-3.1-flash-image"),
		MaxIters:         envInt("PARALLAX_MAX_ITERS", DefaultMaxIters),
		FFmpegBin:        envOr("FFMPEG_BIN", "ffmpeg"),
		FFprobeBin:       envOr("FFPROBE_BIN", "ffprobe"),
		FFmpegHWAccel:    strings.ToLower(envOr("FFMPEG_HWACCEL", "auto")),
		FFmpegHWDevice:   strings.TrimSpace(os.Getenv("FFMPEG_HWDEVICE")),
		LLMs:             LoadLLMProfiles(),
		Embedding: Embedding{
			BaseURL: strings.TrimRight(strings.TrimSpace(os.Getenv("EMBEDDING_BASE_URL")), "/"),
			APIKey:  strings.TrimSpace(os.Getenv("EMBEDDING_API_KEY")),
			Model:   strings.TrimSpace(os.Getenv("EMBEDDING_MODEL")),
		},
		QdrantURL:      strings.TrimRight(envOr("QDRANT_URL", "http://127.0.0.1:6333"), "/"),
		QdrantAPIKey:   strings.TrimSpace(os.Getenv("QDRANT_API_KEY")),
		WhisperModel:   envOr("WHISPER_MODEL", "large-v3-turbo"),
		WhisperDevice:  strings.ToLower(envOr("WHISPER_DEVICE", "auto")),
		WhisperPython:  resolveExisting(envOr("WHISPER_PYTHON", filepath.Join("scripts", ".venv", "bin", "python")), cwd),
		WhisperScript:  resolveExisting(envOr("WHISPER_SCRIPT", filepath.Join("scripts", "transcribe.py")), cwd),
		WhisperCompute: envOr("WHISPER_COMPUTE", "int8"),
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

// PublicProfile is the client-safe view of one configured model.
type PublicProfile struct {
	ID        string `json:"id"`
	Label     string `json:"label,omitempty"`
	BaseURL   string `json:"base_url"`
	Model     string `json:"model"`
	APIKeySet bool   `json:"api_key_set"`
}

// Public is the JSON shape returned to clients. API keys are never exposed.
type Public struct {
	ActiveID  string          `json:"active_id"`
	BaseURL   string          `json:"base_url"`
	Model     string          `json:"model"`
	APIKeySet bool            `json:"api_key_set"`
	Profiles  []PublicProfile `json:"profiles"`
}

func (l LLM) publicProfile() PublicProfile {
	return PublicProfile{
		ID:        l.ID,
		Label:     l.Label,
		BaseURL:   l.BaseURL,
		Model:     l.Model,
		APIKeySet: strings.TrimSpace(l.APIKey) != "",
	}
}

func (l LLM) public() Public {
	return Public{
		ActiveID:  l.ID,
		BaseURL:   l.BaseURL,
		Model:     l.Model,
		APIKeySet: strings.TrimSpace(l.APIKey) != "",
	}
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

// ValidateEmbedding checks the dedicated embeddings endpoint.
func ValidateEmbedding(e Embedding) error {
	if strings.TrimSpace(e.BaseURL) == "" {
		return errors.New("embedding base_url is required")
	}
	if strings.TrimSpace(e.Model) == "" {
		return errors.New("embedding model is required")
	}
	if strings.TrimSpace(e.APIKey) == "" {
		return errors.New("embedding api_key is required")
	}
	if !strings.HasPrefix(e.BaseURL, "http://") && !strings.HasPrefix(e.BaseURL, "https://") {
		return errors.New("embedding base_url must start with http:// or https://")
	}
	return nil
}

// Store holds env-defined LLM profiles and persists only the active selection.
type Store struct {
	mu       sync.RWMutex
	activeID string
	profiles []LLM
	path     string
}

func NewStore(path string, profiles []LLM) *Store {
	cleaned := normalizeProfiles(profiles)
	if len(cleaned) == 0 {
		cleaned = []LLM{{
			ID:      defaultProfileID,
			BaseURL: DefaultBaseURL,
			Model:   DefaultModel,
		}}
	}
	s := &Store{
		activeID: cleaned[0].ID,
		profiles: cleaned,
		path:     path,
	}
	if id, err := readActiveID(path); err == nil && hasProfileID(s.profiles, id) {
		s.activeID = id
	}
	return s
}

func (s *Store) Get() LLM {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activeLocked()
}

func (s *Store) GetByID(id string) (LLM, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if strings.TrimSpace(id) == "" {
		return s.activeLocked(), nil
	}
	if p, ok := findProfile(s.profiles, id); ok {
		return p, nil
	}
	return LLM{}, fmt.Errorf("unknown model %q", id)
}

func (s *Store) Snapshot() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotLocked()
}

func (s *Store) Public() Public {
	s.mu.RLock()
	defer s.mu.RUnlock()
	active := s.activeLocked()
	pub := active.public()
	pub.ActiveID = s.activeID
	pub.Profiles = make([]PublicProfile, 0, len(s.profiles))
	for _, p := range s.profiles {
		pub.Profiles = append(pub.Profiles, p.publicProfile())
	}
	return pub
}

// Select makes an existing env-defined profile active.
func (s *Store) Select(id string) (LLM, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id = strings.TrimSpace(id)
	p, ok := findProfile(s.profiles, id)
	if !ok {
		return LLM{}, fmt.Errorf("unknown model %q", id)
	}
	s.activeID = p.ID
	if err := writeActiveID(s.path, p.ID); err != nil {
		return LLM{}, err
	}
	return p, nil
}

func (s *Store) activeLocked() LLM {
	if p, ok := findProfile(s.profiles, s.activeID); ok {
		return p
	}
	if len(s.profiles) > 0 {
		return s.profiles[0]
	}
	return LLM{}
}

func (s *Store) snapshotLocked() Settings {
	return Settings{
		ActiveID: s.activeID,
		Profiles: append([]LLM(nil), s.profiles...),
	}
}

type persisted struct {
	ActiveID string `json:"active_id,omitempty"`
}

func readActiveID(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var p persisted
	if err := json.Unmarshal(b, &p); err != nil {
		return "", err
	}
	id := strings.TrimSpace(p.ActiveID)
	if id == "" {
		return "", errors.New("no active model")
	}
	return id, nil
}

func writeActiveID(path, id string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(persisted{ActiveID: id}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// LoadLLMProfiles reads model triples from LLM_MODELS / LLM_PROFILES / the
// single-model fallback vars.
func LoadLLMProfiles() []LLM {
	if raw := strings.TrimSpace(os.Getenv("LLM_PROFILES")); raw != "" {
		var items []LLM
		if err := json.Unmarshal([]byte(raw), &items); err == nil && len(items) > 0 {
			return normalizeProfiles(items)
		}
	}

	if names := strings.TrimSpace(os.Getenv("LLM_MODELS")); names != "" {
		var out []LLM
		for _, name := range strings.Split(names, ",") {
			id := strings.TrimSpace(name)
			if id == "" {
				continue
			}
			prefix := "LLM_" + envID(id)
			out = append(out, LLM{
				ID:      id,
				Label:   strings.TrimSpace(os.Getenv(prefix + "_LABEL")),
				BaseURL: strings.TrimSpace(os.Getenv(prefix + "_BASE_URL")),
				APIKey:  firstNonEmpty(os.Getenv(prefix+"_API_KEY"), os.Getenv(prefix+"_KEY")),
				Model:   strings.TrimSpace(os.Getenv(prefix + "_MODEL")),
			})
		}
		if len(out) > 0 {
			return normalizeProfiles(out)
		}
	}

	return normalizeProfiles([]LLM{{
		ID:      defaultProfileID,
		BaseURL: envOr("LLM_BASE_URL", DefaultBaseURL),
		APIKey:  firstNonEmpty(os.Getenv("LLM_API_KEY"), os.Getenv("XAI_API_KEY")),
		Model:   envOr("LLM_MODEL", DefaultModel),
	}})
}

func normalizeProfiles(profiles []LLM) []LLM {
	out := make([]LLM, 0, len(profiles))
	seen := make(map[string]bool, len(profiles))
	for i, raw := range profiles {
		fallback := defaultProfileID
		if i > 0 {
			fallback = fmt.Sprintf("model-%d", i+1)
		}
		p := normalizeProfile(raw, fallback)
		if p.ID == "" {
			continue
		}
		if seen[p.ID] {
			p.ID = fmt.Sprintf("%s-%d", p.ID, i+1)
		}
		seen[p.ID] = true
		out = append(out, p)
	}
	return out
}

func normalizeProfile(l LLM, fallbackID string) LLM {
	l.ID = strings.TrimSpace(l.ID)
	if l.ID == "" {
		l.ID = fallbackID
	}
	l.Label = strings.TrimSpace(l.Label)
	l.BaseURL = strings.TrimRight(strings.TrimSpace(l.BaseURL), "/")
	l.APIKey = strings.TrimSpace(l.APIKey)
	l.Model = strings.TrimSpace(l.Model)
	return l
}

func envID(id string) string {
	id = strings.ToUpper(strings.TrimSpace(id))
	var b strings.Builder
	for _, r := range id {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	return b.String()
}

func findProfile(profiles []LLM, id string) (LLM, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return LLM{}, false
	}
	for _, p := range profiles {
		if p.ID == id {
			return p, true
		}
	}
	return LLM{}, false
}

func hasProfileID(profiles []LLM, id string) bool {
	_, ok := findProfile(profiles, id)
	return ok
}

// resolveExisting turns a relative whisper path into an absolute one.
// The server is often started from the repo root, not parallax_backend/.
func resolveExisting(path, cwd string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		return path
	}
	candidates := []string{
		filepath.Join(cwd, path),
		filepath.Join(cwd, "parallax_backend", path),
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates, filepath.Join(dir, path), filepath.Join(dir, "..", path))
	}
	for _, cand := range candidates {
		abs, err := filepath.Abs(cand)
		if err != nil {
			continue
		}
		if _, err := os.Stat(abs); err == nil {
			return abs
		}
	}
	abs, err := filepath.Abs(filepath.Join(cwd, path))
	if err != nil {
		return path
	}
	return abs
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

// LoadDotEnv is a tiny KEY=VALUE reader. Missing files are ignored.
func LoadDotEnv(path string) {
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
