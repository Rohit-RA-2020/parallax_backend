package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"parallax/internal/agent"
	"parallax/internal/auth"
	"parallax/internal/config"
	"parallax/internal/database"
	"parallax/internal/elevenlabs"
	"parallax/internal/ffmpeg"
	"parallax/internal/gemini"
	"parallax/internal/gifs"
	"parallax/internal/llm"
	"parallax/internal/objectstore"
	"parallax/internal/preview"
	"parallax/internal/projects"
	"parallax/internal/tools"
	"parallax/internal/transcript"
	"parallax/internal/visualreview"
)

// ProviderFactory builds a ChatProvider from the current LLM settings.
// Tests inject a fake; production uses the OpenAI-compatible HTTP client.
type ProviderFactory func(cfg config.LLM) llm.ChatProvider

type Server struct {
	Addr                    string
	Settings                *config.Store
	Sessions                *agent.Store
	Tools                   *tools.Registry
	SystemPrompt            string
	ExaAPIKey               string
	ExaBaseURL              string
	GeminiAPIKey            string
	GeminiBaseURL           string
	GeminiImageModel        string
	GeminiOmniVideoModel    string
	GeminiVeoVideoModel     string
	GeminiVideoTimeout      time.Duration
	GeminiVideoPoll         time.Duration
	GIFs                    *gifs.Service
	GeminiMusic             *gemini.Client
	GeminiMusicModel        string
	GeminiMusicOutputFormat string
	Bins                    ffmpeg.Bins
	Projects                *projects.Store
	NewLLM                  ProviderFactory
	MaxIters                int
	MaxParallelTools        int
	Logger                  *slog.Logger
	Workspace               string
	ProviderIconsDir        string
	Indexer                 *transcript.Indexer
	Previews                *preview.Builder
	ElevenLabs              *elevenlabs.Client
	ElevenVoices            *elevenlabs.VoiceCatalog
	ElevenTTSModel          string
	ElevenSFXModel          string
	ElevenTTSOutputFormat   string
	ElevenSFXOutputFormat   string
	ElevenLimiter           *tools.Limiter
	YouTubeTimeout          time.Duration
	YouTubeMaxBytes         int64
	YouTubeYTDLPBin         string
	BlenderBin              string
	BlenderBridgeHost       string
	BlenderBridgePort       int
	BlenderTimeout          time.Duration
	Uploads                 *UploadManager
	Auth                    *auth.Authenticator
	Database                *database.DB
	Objects                 *objectstore.Client
	AllowedOrigins          []string
}

// deferredMediaIndex collects media produced during one Director transaction.
// In the PostgreSQL store, generated files become addressable only when that
// transaction commits and promotes the workspace file into an asset version.
// Starting a worker first makes ResolveFile correctly report the as-yet
// unpromoted asset as missing.
type deferredMediaIndex struct {
	mu     sync.Mutex
	media  map[string]struct{}
	images map[string]string
	audio  map[string]deferredGeneratedAudio
}

type deferredGeneratedAudio struct {
	doc      *transcript.Document
	metadata transcript.GeneratedAudioMetadata
}

func (q *deferredMediaIndex) addMedia(rel string) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.media == nil {
		q.media = map[string]struct{}{}
	}
	q.media[rel] = struct{}{}
}

func (q *deferredMediaIndex) addGeneratedImage(rel, prompt string) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.images == nil {
		q.images = map[string]string{}
	}
	q.images[rel] = prompt
}

func (q *deferredMediaIndex) addGeneratedAudio(rel string, doc *transcript.Document, metadata transcript.GeneratedAudioMetadata) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.audio == nil {
		q.audio = map[string]deferredGeneratedAudio{}
	}
	q.audio[rel] = deferredGeneratedAudio{doc: doc, metadata: metadata}
}

func (q *deferredMediaIndex) flush(s *Server, projectID string) {
	q.mu.Lock()
	media := q.media
	images := q.images
	audio := q.audio
	q.media = nil
	q.images = nil
	q.audio = nil
	q.mu.Unlock()
	for rel := range media {
		// A generated-image request has a caption hint and needs the more
		// specific queueing path below.
		if _, generatedImage := images[rel]; !generatedImage {
			s.indexMedia(projectID, rel)
		}
	}
	for rel, prompt := range images {
		s.indexGeneratedImage(projectID, rel, prompt)
	}
	for rel, generated := range audio {
		s.indexGeneratedAudio(projectID, rel, generated.doc, generated.metadata)
	}
}

func (s *Server) indexMedia(projectID, rel string) {
	if s == nil {
		return
	}
	if s.Indexer != nil {
		s.Indexer.Enqueue(projectID, rel)
	}
	if s.Previews != nil {
		s.Previews.Enqueue(projectID, rel)
	}
}

func (s *Server) enqueuePreview(projectID, rel string) {
	if s == nil || s.Previews == nil {
		return
	}
	s.Previews.Enqueue(projectID, rel)
}

func (s *Server) indexGeneratedImage(projectID, rel, prompt string) {
	if s == nil || s.Indexer == nil {
		return
	}
	s.Indexer.SetImageHint(projectID, rel, prompt)
	s.Indexer.Enqueue(projectID, rel)
}

func (s *Server) indexGeneratedAudio(projectID, rel string, doc *transcript.Document, metadata transcript.GeneratedAudioMetadata) {
	if s == nil || s.Indexer == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
		defer cancel()
		if err := s.Indexer.IndexGeneratedAudio(ctx, projectID, rel, doc, metadata); err != nil {
			s.log().Error("index generated audio", "project", projectID, "path", rel, "err", err)
		}
	}()
}

func (s *Server) indexProject(projectID string) {
	if s == nil || s.Projects == nil {
		return
	}
	media, err := s.Projects.ListMedia(projectID)
	if err != nil {
		return
	}
	for _, item := range media {
		s.indexMedia(projectID, item.Path)
	}
}

func (s *Server) systemPrompt() string {
	if strings.TrimSpace(s.SystemPrompt) != "" {
		return s.SystemPrompt
	}
	return agent.SystemPrompt
}

func (s *Server) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Backward-compatible liveness alias.
	mux.HandleFunc("GET /health", s.handleLiveness)
	mux.HandleFunc("GET /health/live", s.handleLiveness)
	mux.HandleFunc("GET /health/ready", s.handleReadiness)
	mux.HandleFunc("POST /v1/auth/media-session", s.handleMediaSession)
	mux.HandleFunc("DELETE /v1/auth/media-session", s.handleClearMediaSession)
	if s.Auth != nil && s.Objects != nil && s.Database != nil {
		mux.HandleFunc("GET /v1/media/objects/{objectId}", s.handleMediaObject)
		mux.HandleFunc("HEAD /v1/media/objects/{objectId}", s.handleMediaObject)
		mux.HandleFunc("GET /v1/media/{versionId}", s.handleMediaContent)
		mux.HandleFunc("HEAD /v1/media/{versionId}", s.handleMediaContent)
	}
	if s.Uploads != nil {
		mux.Handle("/v1/uploads", http.StripPrefix("/v1/uploads", s.Uploads.Handler()))
		mux.Handle("/v1/uploads/", http.StripPrefix("/v1/uploads", s.Uploads.Handler()))
		mux.HandleFunc("GET /v1/upload-status/{id}", s.handleUploadStatus)
	}
	mux.HandleFunc("GET /v1/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /v1/settings", s.handlePutSettings)
	// Provider logos configured via LLM_<ID>_PROVIDER_ICON_* are served by
	// the backend from ./provider-icons (PARALLAX_PROVIDER_ICONS_DIR).
	// The frontend proxies /provider-icons/ here in dev (vite) and prod
	// (nginx), so a backend-only drop-in works with no frontend rebuild.
	mux.HandleFunc("GET /provider-icons/", s.handleProviderIcons)
	mux.HandleFunc("HEAD /provider-icons/", s.handleProviderIcons)
	if s.GIFs != nil {
		mux.HandleFunc("GET /v1/gifs/search", s.handleSearchGIFs)
	}
	mux.HandleFunc("POST /v1/agent/chat", s.handleChat)
	mux.HandleFunc("GET /v1/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.handleDeleteSession)
	if s.Projects != nil {
		mux.HandleFunc("GET /v1/projects", s.handleListProjects)
		mux.HandleFunc("POST /v1/projects", s.handleCreateProject)
		mux.HandleFunc("GET /v1/projects/{id}", s.projectAuthorized(s.handleGetProject))
		mux.HandleFunc("DELETE /v1/projects/{id}", s.projectAuthorized(s.handleDeleteProject))
		mux.HandleFunc("GET /v1/projects/{id}/media/search", s.projectAuthorized(s.handleSearchMedia))
		mux.HandleFunc("GET /v1/projects/{id}/media", s.projectAuthorized(s.handleListMedia))
		mux.HandleFunc("POST /v1/projects/{id}/media/describe", s.projectAuthorized(s.handleDescribeMedia))
		if s.GIFs != nil {
			mux.HandleFunc("POST /v1/projects/{id}/gifs/import", s.projectAuthorized(s.handleImportGIF))
		}
		mux.HandleFunc("POST /v1/projects/{id}/export", s.projectAuthorized(s.handleExport))
		mux.HandleFunc("GET /v1/projects/{id}/files/{path...}", s.projectAuthorized(s.handleProjectFile))
		mux.HandleFunc("DELETE /v1/projects/{id}/files/{path...}", s.projectAuthorized(s.handleDeleteProjectFile))
		mux.HandleFunc("GET /v1/projects/{id}/chats", s.projectAuthorized(s.handleListChats))
		mux.HandleFunc("POST /v1/projects/{id}/chats", s.projectAuthorized(s.handleCreateChat))
		mux.HandleFunc("GET /v1/projects/{id}/chats/{chatId}", s.projectAuthorized(s.handleGetChat))
		mux.HandleFunc("PATCH /v1/projects/{id}/chats/{chatId}", s.projectAuthorized(s.handlePatchChat))
		mux.HandleFunc("DELETE /v1/projects/{id}/chats/{chatId}", s.projectAuthorized(s.handleDeleteChat))
		mux.HandleFunc("GET /v1/projects/{id}/timeline", s.projectAuthorized(s.handleGetTimeline))
		mux.HandleFunc("PUT /v1/projects/{id}/timeline", s.projectAuthorized(s.handlePutTimeline))
		mux.HandleFunc("POST /v1/projects/{id}/visual-review", s.projectAuthorized(s.handleVisualReview))
		mux.HandleFunc("GET /v1/projects/{id}/visual-reviews/{revision}", s.projectAuthorized(s.handleGetVisualReview))
		mux.HandleFunc("GET /v1/projects/{id}/history", s.projectAuthorized(s.handleGetHistory))
		mux.HandleFunc("POST /v1/projects/{id}/history/undo", s.projectAuthorized(s.handleUndoHistory))
		mux.HandleFunc("POST /v1/projects/{id}/history/redo", s.projectAuthorized(s.handleRedoHistory))
		mux.HandleFunc("POST /v1/projects/{id}/history/restore", s.projectAuthorized(s.handleRestoreHistory))
		mux.HandleFunc("POST /v1/projects/{id}/checkpoints", s.projectAuthorized(s.handleCreateCheckpoint))
		mux.HandleFunc("PATCH /v1/projects/{id}/checkpoints/{checkpoint}", s.projectAuthorized(s.handleRenameCheckpoint))
		mux.HandleFunc("DELETE /v1/projects/{id}/checkpoints/{checkpoint}", s.projectAuthorized(s.handleDeleteCheckpoint))
	}
	var handler http.Handler = mux
	if s.Auth != nil {
		handler = s.authenticateExceptPublic(handler)
	}
	return withCORS(s.AllowedOrigins, withLog(s.log(), handler))
}

func (s *Server) authenticateExceptPublic(next http.Handler) http.Handler {
	protected := s.Auth.Middleware(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/health" || strings.HasPrefix(path, "/health/") || strings.HasPrefix(path, "/v1/media/") || strings.HasPrefix(path, "/provider-icons/") {
			next.ServeHTTP(w, r)
			return
		}
		protected.ServeHTTP(w, r)
	})
}

func (s *Server) projectAuthorized(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Auth != nil && s.Database != nil {
			user, ok := auth.UserFrom(r.Context())
			if !ok {
				writeError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			var exists bool
			err := s.Database.Pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM projects WHERE id=$1 AND owner_id=$2 AND state='active')`, r.PathValue("id"), user.ID).Scan(&exists)
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, "database unavailable")
				return
			}
			if !exists {
				writeError(w, http.StatusNotFound, "project not found")
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	llmCfg := s.Settings.Get()
	body := map[string]any{
		"ok":        true,
		"model":     llmCfg.Model,
		"base_url":  llmCfg.BaseURL,
		"workspace": s.Workspace,
	}
	if s.Uploads != nil {
		body["uploads"] = s.Uploads.Stats()
	}
	if s.Indexer != nil {
		body["index_queue_depth"] = s.Indexer.QueueDepth()
	}
	if s.Previews != nil {
		body["preview_queue_depth"] = s.Previews.QueueDepth()
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleLiveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var unavailable []string
	if s.Database != nil {
		if err := s.Database.Ready(ctx); err != nil {
			unavailable = append(unavailable, "postgres")
		}
	}
	if s.Objects != nil {
		if err := s.Objects.Ready(ctx); err != nil {
			unavailable = append(unavailable, "media_storage")
		}
	}
	if s.Indexer == nil || s.Indexer.Qdrant == nil {
		writeError(w, http.StatusServiceUnavailable, "qdrant is not configured")
		return
	}
	if err := s.Indexer.Qdrant.Ready(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "qdrant is unavailable")
		return
	}
	if len(unavailable) > 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "unavailable": unavailable})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	public := s.Settings.Public()
	if s.Database != nil {
		if user, ok := auth.UserFrom(r.Context()); ok {
			var active, effort string
			err := s.Database.Pool.QueryRow(r.Context(), `SELECT active_llm_profile_id,thinking_effort FROM user_preferences WHERE user_id=$1`, user.ID).Scan(&active, &effort)
			if err == nil && active != "" {
				public.ActiveID = active
				if profile, pErr := s.Settings.GetByID(active); pErr == nil {
					public.BaseURL = profile.BaseURL
					public.Model = profile.Model
					public.APIKeySet = strings.TrimSpace(profile.APIKey) != ""
				}
			}
			if effort != "" {
				public.ThinkingEffort = effort
			}
		}
	}
	writeJSON(w, http.StatusOK, public)
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ActiveID       string `json:"active_id"`
		ThinkingEffort string `json:"thinking_effort"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(body.ActiveID) == "" {
		writeError(w, http.StatusBadRequest, "active_id is required")
		return
	}
	if s.Database != nil {
		user, ok := auth.UserFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if _, err := s.Settings.GetByID(body.ActiveID); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		effort := strings.TrimSpace(body.ThinkingEffort)
		hasEffort := effort != ""
		if effort == "" {
			effort = "medium"
		}
		if _, err := llm.NormalizeThinkingEffort(effort); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		_, err := s.Database.Pool.Exec(r.Context(), `INSERT INTO user_preferences(user_id,active_llm_profile_id,thinking_effort) VALUES($1,$2,$3) ON CONFLICT(user_id) DO UPDATE SET active_llm_profile_id=EXCLUDED.active_llm_profile_id,thinking_effort=CASE WHEN $4 THEN EXCLUDED.thinking_effort ELSE user_preferences.thinking_effort END,updated_at=now()`, user.ID, body.ActiveID, effort, hasEffort)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}
		s.handleGetSettings(w, r)
		return
	}
	if _, err := s.Settings.Select(body.ActiveID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.Settings.Public())
}

type chatRequest struct {
	SessionID      string        `json:"session_id"`
	ProjectID      string        `json:"project_id"`
	ProfileID      string        `json:"profile_id"`
	Message        string        `json:"message"`
	Messages       []llm.Message `json:"messages"`
	Images         []chatImageIn `json:"images"`
	ThinkingEffort string        `json:"thinking_effort"`
}

type chatImageIn struct {
	Name string `json:"name"`
	MIME string `json:"mime"`
	Data string `json:"data"`
}

const (
	maxChatImages     = 6
	maxChatImageBytes = 8 << 20
)

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	requestStartedAt := time.Now()
	systemPrompt := s.systemPrompt()
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	userText := strings.TrimSpace(req.Message)
	if userText == "" && len(req.Messages) == 0 && len(req.Images) == 0 {
		writeError(w, http.StatusBadRequest, "message is required")
		return
	}

	if (strings.TrimSpace(req.ProfileID) == "" || strings.TrimSpace(req.ThinkingEffort) == "") && s.Database != nil {
		if user, ok := auth.UserFrom(r.Context()); ok {
			var profileID, effort string
			if s.Database.Pool.QueryRow(r.Context(), `SELECT active_llm_profile_id,thinking_effort FROM user_preferences WHERE user_id=$1`, user.ID).Scan(&profileID, &effort) == nil {
				if strings.TrimSpace(req.ProfileID) == "" {
					req.ProfileID = profileID
				}
				if strings.TrimSpace(req.ThinkingEffort) == "" {
					req.ThinkingEffort = effort
				}
			}
		}
	}
	llmCfg, err := s.Settings.GetByID(req.ProfileID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := config.ValidateLLM(llmCfg); err != nil {
		writeError(w, http.StatusFailedDependency, "LLM is not configured: "+err.Error())
		return
	}
	thinkingEffort, err := llm.NormalizeThinkingEffort(req.ThinkingEffort)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	provider := s.NewLLM(llmCfg)

	toolRegistry := s.Tools
	projectID := strings.TrimSpace(req.ProjectID)
	if s.Auth != nil && projectID == "" {
		writeError(w, http.StatusBadRequest, "project_id is required")
		return
	}
	if s.Auth != nil && projectID != "" {
		user, ok := auth.UserFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if _, err := s.Projects.ForOwner(user.ID).Get(projectID); err != nil {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
	}
	attached, attachErr := s.saveChatImages(projectID, req.Images)
	if attachErr != nil {
		writeError(w, http.StatusBadRequest, attachErr.Error())
		return
	}
	attachedPaths := make([]string, 0, len(attached))
	for _, image := range attached {
		if path := strings.TrimSpace(image.Path); path != "" {
			attachedPaths = append(attachedPaths, path)
			s.indexMedia(projectID, path)
		}
	}
	var timelineTx *projects.TimelineTransaction
	var deferredIndex *deferredMediaIndex
	if projectID != "" {
		if s.Projects == nil {
			writeError(w, http.StatusBadRequest, "projects are not configured")
			return
		}
		project, err := s.Projects.Get(projectID)
		if err != nil {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		toolRegistry = tools.NewRegistry()
		deferredIndex = &deferredMediaIndex{}
		summary := userText
		if summary == "" && len(attached) > 0 {
			summary = "Attached image"
		}
		timelineTx, err = s.Projects.BeginTimelineTransaction(projectID, projects.CommitMeta{
			Actor: "agent", Summary: summary, ChatID: strings.TrimSpace(req.SessionID),
		})
		if err != nil {
			writeProjectError(w, err)
			return
		}
		publishMedia := func(rel string) {
			if err := s.Projects.PublishWorkspaceMedia(r.Context(), projectID, rel); err != nil {
				s.log().Error("publish live media", "project", projectID, "path", rel, "err", err)
			}
		}
		mediaApplied := func(rel string) {
			publishMedia(rel)
			deferredIndex.addMedia(rel)
		}
		imageApplied := func(rel, prompt string) {
			publishMedia(rel)
			deferredIndex.addGeneratedImage(rel, prompt)
		}
		audioApplied := func(rel string, doc *transcript.Document, metadata transcript.GeneratedAudioMetadata) {
			publishMedia(rel)
			deferredIndex.addGeneratedAudio(rel, doc, metadata)
		}

		tools.RegisterMedia(toolRegistry, tools.MediaEnv{
			Workspace:  project.Dir,
			Bins:       s.Bins,
			OnMutation: timelineTx.MarkMediaMutation,
			OnApplied:  mediaApplied,
		})
		tools.RegisterWeb(toolRegistry, tools.WebEnv{APIKey: s.ExaAPIKey, BaseURL: s.ExaBaseURL})
		tools.RegisterGIFs(toolRegistry, tools.GIFEnv{
			Service: s.GIFs, Projects: s.Projects, ProjectID: projectID,
			OnMutation: timelineTx.MarkMediaMutation,
			OnApplied:  mediaApplied,
		})
		tools.RegisterYouTube(toolRegistry, tools.YouTubeEnv{
			Workspace: project.Dir, Bins: s.Bins, Timeout: s.YouTubeTimeout, MaxBytes: s.YouTubeMaxBytes, YTDLPBin: s.YouTubeYTDLPBin,
			OnMutation: timelineTx.MarkMediaMutation,
			OnApplied:  mediaApplied,
		})
		tools.RegisterImage(toolRegistry, tools.ImageEnv{
			Workspace:     project.Dir,
			APIKey:        s.GeminiAPIKey,
			BaseURL:       s.GeminiBaseURL,
			Model:         s.GeminiImageModel,
			DefaultImages: attachedPaths,
			OnMutation:    timelineTx.MarkMediaMutation,
			OnApplied:     imageApplied,
		})
		videoClient := gemini.NewClient(s.GeminiAPIKey, s.GeminiBaseURL, s.GeminiVideoTimeout, 256<<20)
		tools.RegisterVideoGeneration(toolRegistry, tools.VideoGenerationEnv{
			Workspace:     project.Dir,
			Bins:          s.Bins,
			Client:        videoClient,
			OmniModel:     s.GeminiOmniVideoModel,
			VeoModel:      s.GeminiVeoVideoModel,
			DefaultImages: attachedPaths,
			Poll:          s.GeminiVideoPoll,
			OnMutation:    timelineTx.MarkMediaMutation,
			OnApplied:     mediaApplied,
		})
		tools.RegisterAudioGeneration(toolRegistry, tools.AudioGenerationEnv{
			Workspace: project.Dir, Bins: s.Bins, Client: s.ElevenLabs, Voices: s.ElevenVoices,
			MusicClient: s.GeminiMusic, GeminiMusicModel: s.GeminiMusicModel, GeminiMusicOutputFormat: s.GeminiMusicOutputFormat,
			TTSModel: s.ElevenTTSModel, SFXModel: s.ElevenSFXModel,
			TTSOutputFormat: s.ElevenTTSOutputFormat, SFXOutputFormat: s.ElevenSFXOutputFormat,
			Limiter: s.ElevenLimiter, ProjectID: projectID, Transaction: timelineTx, Indexer: s.Indexer,
			OnIndex: audioApplied,
			Logger:  s.Logger, OnMutation: timelineTx.MarkMediaMutation,
		})
		tools.RegisterTimeline(toolRegistry, tools.TimelineEnv{
			Transaction: timelineTx,
			Store:       s.Projects,
			ProjectID:   projectID,
			Workspace:   project.Dir,
			Bins:        s.Bins,
			Review:      &visualreview.Service{Store: s.Projects, Bins: s.Bins, Vision: provider, RenderWidth: 960, RenderHeight: 540},
		})
		tools.RegisterTranscript(toolRegistry, tools.TranscriptEnv{
			Indexer:     s.Indexer,
			ProjectID:   projectID,
			Workspace:   project.Dir,
			Bins:        s.Bins,
			Transaction: timelineTx,
			OnMutation:  timelineTx.MarkMediaMutation,
			OnApplied:   mediaApplied,
		})
		tools.RegisterBlender(toolRegistry, tools.BlenderEnv{
			Workspace: project.Dir, BlenderBin: s.BlenderBin,
			BridgeHost: s.BlenderBridgeHost, BridgePort: s.BlenderBridgePort,
			Timeout:    s.BlenderTimeout,
			OnMutation: timelineTx.MarkMediaMutation,
			OnApplied:  mediaApplied,
		})
	}
	if toolRegistry == nil {
		writeError(w, http.StatusInternalServerError, "media tools are not configured")
		return
	}

	var sess *agent.Session
	var titleResult <-chan string
	if projectID != "" {
		chat, err := s.Projects.GetOrCreateChat(projectID, strings.TrimSpace(req.SessionID))
		if err != nil {
			writeProjectError(w, err)
			return
		}
		sess = &agent.Session{
			ID:        chat.ID,
			ProjectID: projectID,
			Messages:  chat.Messages,
			UpdatedAt: chat.UpdatedAt,
		}
		s.Sessions.Remember(sess)
		if isFirstChatMessage(chat.Messages) && isDefaultGeneratedTitle(chat.Title) && (userText != "" || len(attached) > 0) {
			seed := userText
			if seed == "" && len(attached) > 0 {
				seed = "Attached image: " + attached[0].Name
			}
			titleResult = s.generateChatTitle(r.Context(), llmCfg, seed)
		}
	} else {
		sess = s.Sessions.GetOrCreateForProject(req.SessionID, "")
	}
	if timelineTx != nil {
		timelineTx.SetChatID(sess.ID)
	}
	msgs := append([]llm.Message(nil), sess.Messages...)
	if len(msgs) == 0 || msgs[0].Role != llm.RoleSystem {
		msgs = append([]llm.Message{{Role: llm.RoleSystem, Content: systemPrompt}}, msgs...)
	} else {
		msgs[0].Content = systemPrompt
	}
	if len(req.Messages) > 0 {
		// Caller-supplied history replaces the conversation but keeps the system prompt.
		msgs = []llm.Message{{Role: llm.RoleSystem, Content: msgs[0].Content}}
		for _, m := range req.Messages {
			if m.Role == llm.RoleSystem {
				continue
			}
			msgs = append(msgs, m)
		}
	}
	if userText != "" || len(attached) > 0 {
		lastUser := false
		if n := len(msgs); n > 0 && msgs[n-1].Role == llm.RoleUser && msgs[n-1].Content == userText && len(attached) == 0 {
			lastUser = true
		}
		if !lastUser {
			msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: userText, Images: attached})
		}
	}
	if projectID != "" {
		if saved, err := s.Projects.SaveChatMessages(projectID, sess.ID, msgs); err == nil {
			sess.UpdatedAt = saved.UpdatedAt
		}
	}

	stream, err := newSSE(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = stream.Event(agent.NewEvent(agent.EventSession, agent.SessionPayload{SessionID: sess.ID}))

	if c, ok := provider.(*llm.CompatClient); ok && c != nil {
		if c.ExtraHeaders == nil {
			c.ExtraHeaders = map[string]string{}
		}
		c.ExtraHeaders["x-grok-conv-id"] = sess.ID
	}
	var traceEvents []projects.ChatTraceEvent

	if projectID != "" && s.Projects != nil {
		msgs = llm.HydrateMessageImages(msgs, func(rel string) ([]byte, error) {
			abs, err := s.Projects.ResolveFile(projectID, rel)
			if err != nil {
				return nil, err
			}
			return os.ReadFile(abs)
		})
	}

	ag := &agent.Agent{
		Provider:         provider,
		Tools:            toolRegistry,
		MaxIters:         s.MaxIters,
		MaxParallelTools: s.MaxParallelTools,
		Logger:           s.log(),
	}
	out := ag.Run(r.Context(), agent.Input{
		SessionID:      sess.ID,
		Messages:       msgs,
		ThinkingEffort: thinkingEffort,
	}, func(ev agent.Event) {
		if ev.Type != agent.EventText && ev.Type != agent.EventSession && ev.Type != agent.EventProjectChanged {
			traceEvents = appendTraceEvent(traceEvents, ev)
		}
		_ = stream.Event(ev)
	})
	if titleResult != nil {
		if title := <-titleResult; title != "" {
			if renamed, renameErr := s.Projects.RenameChat(projectID, sess.ID, title); renameErr != nil {
				s.log().Error("persist generated chat title", "project", projectID, "chat", sess.ID, "err", renameErr)
			} else {
				_ = stream.Event(agent.NewEvent(agent.EventChatTitle, agent.ChatTitlePayload{SessionID: sess.ID, Title: renamed.Title}))
			}
		}
	}
	if timelineTx != nil {
		if out.Reason == "error" || out.Reason == "canceled" || out.Reason == "max_iterations" {
			timelineTx.Rollback()
		} else {
			timeline, changed, commitErr := timelineTx.Commit()
			if commitErr != nil {
				_ = stream.Event(agent.NewEvent(agent.EventError, agent.ErrorPayload{Message: "timeline commit failed: " + commitErr.Error()}))
			} else if changed {
				_ = stream.Event(agent.NewEvent(agent.EventProjectChanged, agent.ProjectChangedPayload{
					ProjectID: projectID, Revision: timeline.Revision, TimelineChanged: true,
				}))
			}
			if commitErr == nil {
				deferredIndex.flush(s, projectID)
			}
		}
	}
	s.Sessions.ReplaceMessages(sess.ID, out.Messages)
	if projectID != "" {
		if _, err := s.Projects.SaveChatMessages(projectID, sess.ID, out.Messages); err != nil {
			s.log().Error("persist chat", "project", projectID, "chat", sess.ID, "err", err)
		}
		if out.Reason != "error" && out.Reason != "canceled" && out.Reason != "max_iterations" {
			if err := s.Projects.SetChatResponseMetadata(projectID, sess.ID, out.Messages, time.Since(requestStartedAt).Milliseconds(), traceEvents); err != nil {
				s.log().Error("persist response duration", "project", projectID, "chat", sess.ID, "err", err)
			}
		}
		_ = s.Projects.Touch(projectID)
	}
}

func appendTraceEvent(events []projects.ChatTraceEvent, ev agent.Event) []projects.ChatTraceEvent {
	if ev.Type == agent.EventToolProgress {
		return events
	}
	if ev.Type != agent.EventThinking {
		return append(events, projects.ChatTraceEvent{Type: string(ev.Type), Data: append([]byte(nil), ev.Data...)})
	}
	var incoming agent.ThinkingPayload
	if json.Unmarshal(ev.Data, &incoming) != nil {
		return events
	}
	if incoming.Delta == "" && incoming.Text == "" {
		return events
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != string(agent.EventThinking) {
			continue
		}
		var existing agent.ThinkingPayload
		if json.Unmarshal(events[i].Data, &existing) != nil || existing.Iteration != incoming.Iteration {
			break
		}
		if incoming.Text != "" {
			existing.Text = incoming.Text
		} else {
			existing.Text += incoming.Delta
		}
		existing.Delta = ""
		raw, err := json.Marshal(existing)
		if err != nil {
			return events
		}
		events[i].Data = raw
		return events
	}
	stored := agent.ThinkingPayload{Text: incoming.Text, Iteration: incoming.Iteration}
	if stored.Text == "" {
		stored.Text = incoming.Delta
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		return events
	}
	return append(events, projects.ChatTraceEvent{Type: string(ev.Type), Data: raw})
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.Sessions.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if s.Auth != nil {
		user, hasUser := auth.UserFrom(r.Context())
		var owned bool
		if !hasUser || s.Database.Pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM projects WHERE id=$1 AND owner_id=$2 AND state='active')`, sess.ProjectID, user.ID).Scan(&owned) != nil || !owned {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         sess.ID,
		"updated_at": sess.UpdatedAt,
		"messages":   agent.PublicHistory(sess.Messages),
	})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if s.Auth != nil {
		sess, ok := s.Sessions.Get(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		user, hasUser := auth.UserFrom(r.Context())
		var owned bool
		if !hasUser || s.Database.Pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM projects WHERE id=$1 AND owner_id=$2 AND state='active')`, sess.ProjectID, user.ID).Scan(&owned) != nil || !owned {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
	}
	s.Sessions.Delete(r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

func withCORS(allowed []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		origin := r.Header.Get("Origin")
		if origin != "" && originAllowed(origin, allowed) {
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Add("Vary", "Origin")
		}
		h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Expected-Revision, X-Change-Summary, Range, Tus-Resumable, Upload-Length, Upload-Metadata, Upload-Offset, Upload-Defer-Length")
		h.Set("Access-Control-Allow-Methods", "GET, HEAD, PUT, POST, PATCH, DELETE, OPTIONS")
		h.Set("Access-Control-Expose-Headers", "Accept-Ranges, Content-Range, Content-Length, Content-Type, Upload-Offset, Upload-Length, Location, Tus-Resumable")
		if r.Method == http.MethodOptions {
			if strings.HasPrefix(r.URL.Path, "/v1/uploads") {
				h.Set("Tus-Version", "1.0.0")
				h.Set("Tus-Extension", "termination")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func originAllowed(origin string, allowed []string) bool {
	if len(allowed) == 0 {
		return origin == "http://localhost:5173" || origin == "http://127.0.0.1:5173"
	}
	for _, item := range allowed {
		if item == origin {
			return true
		}
	}
	return false
}

func withLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"dur", time.Since(start).Round(time.Millisecond).String(),
		)
	})
}
