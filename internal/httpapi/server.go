package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"parallax/internal/agent"
	"parallax/internal/config"
	"parallax/internal/elevenlabs"
	"parallax/internal/ffmpeg"
	"parallax/internal/gemini"
	"parallax/internal/llm"
	"parallax/internal/projects"
	"parallax/internal/tools"
	"parallax/internal/transcript"
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
	GeminiMusic             *gemini.Client
	GeminiMusicModel        string
	GeminiMusicOutputFormat string
	Bins                    ffmpeg.Bins
	Projects                *projects.Store
	NewLLM                  ProviderFactory
	MaxIters                int
	Logger                  *slog.Logger
	Workspace               string
	Indexer                 *transcript.Indexer
	ElevenLabs              *elevenlabs.Client
	ElevenVoices            *elevenlabs.VoiceCatalog
	ElevenTTSModel          string
	ElevenSFXModel          string
	ElevenTTSOutputFormat   string
	ElevenSFXOutputFormat   string
	ElevenLimiter           *tools.Limiter
}

func (s *Server) indexMedia(projectID, rel string) {
	if s == nil || s.Indexer == nil {
		return
	}
	s.Indexer.Enqueue(projectID, rel)
}

func (s *Server) indexGeneratedImage(projectID, rel, prompt string) {
	if s == nil || s.Indexer == nil {
		return
	}
	s.Indexer.SetImageHint(projectID, rel, prompt)
	s.Indexer.Enqueue(projectID, rel)
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
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /v1/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /v1/settings", s.handlePutSettings)
	mux.HandleFunc("POST /v1/agent/chat", s.handleChat)
	mux.HandleFunc("GET /v1/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.handleDeleteSession)
	if s.Projects != nil {
		mux.HandleFunc("GET /v1/projects", s.handleListProjects)
		mux.HandleFunc("POST /v1/projects", s.handleCreateProject)
		mux.HandleFunc("GET /v1/projects/{id}", s.handleGetProject)
		mux.HandleFunc("DELETE /v1/projects/{id}", s.handleDeleteProject)
		mux.HandleFunc("GET /v1/projects/{id}/media/search", s.handleSearchMedia)
		mux.HandleFunc("GET /v1/projects/{id}/media", s.handleListMedia)
		mux.HandleFunc("POST /v1/projects/{id}/media", s.handleUploadMedia)
		mux.HandleFunc("POST /v1/projects/{id}/export", s.handleExport)
		mux.HandleFunc("GET /v1/projects/{id}/files/{path...}", s.handleProjectFile)
		mux.HandleFunc("DELETE /v1/projects/{id}/files/{path...}", s.handleDeleteProjectFile)
		mux.HandleFunc("GET /v1/projects/{id}/chats", s.handleListChats)
		mux.HandleFunc("POST /v1/projects/{id}/chats", s.handleCreateChat)
		mux.HandleFunc("GET /v1/projects/{id}/chats/{chatId}", s.handleGetChat)
		mux.HandleFunc("PATCH /v1/projects/{id}/chats/{chatId}", s.handlePatchChat)
		mux.HandleFunc("DELETE /v1/projects/{id}/chats/{chatId}", s.handleDeleteChat)
		mux.HandleFunc("GET /v1/projects/{id}/timeline", s.handleGetTimeline)
		mux.HandleFunc("PUT /v1/projects/{id}/timeline", s.handlePutTimeline)
		mux.HandleFunc("GET /v1/projects/{id}/history", s.handleGetHistory)
		mux.HandleFunc("POST /v1/projects/{id}/history/undo", s.handleUndoHistory)
		mux.HandleFunc("POST /v1/projects/{id}/history/redo", s.handleRedoHistory)
		mux.HandleFunc("POST /v1/projects/{id}/history/restore", s.handleRestoreHistory)
		mux.HandleFunc("POST /v1/projects/{id}/checkpoints", s.handleCreateCheckpoint)
		mux.HandleFunc("PATCH /v1/projects/{id}/checkpoints/{checkpoint}", s.handleRenameCheckpoint)
		mux.HandleFunc("DELETE /v1/projects/{id}/checkpoints/{checkpoint}", s.handleDeleteCheckpoint)
	}
	return withCORS(withLog(s.log(), mux))
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	llmCfg := s.Settings.Get()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"model":     llmCfg.Model,
		"base_url":  llmCfg.BaseURL,
		"workspace": s.Workspace,
	})
}

func (s *Server) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Settings.Public())
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ActiveID string `json:"active_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(body.ActiveID) == "" {
		writeError(w, http.StatusBadRequest, "active_id is required")
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

	toolRegistry := s.Tools
	projectID := strings.TrimSpace(req.ProjectID)
	attached, attachErr := s.saveChatImages(projectID, req.Images)
	if attachErr != nil {
		writeError(w, http.StatusBadRequest, attachErr.Error())
		return
	}
	var timelineTx *projects.TimelineTransaction
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
		tools.RegisterMedia(toolRegistry, tools.MediaEnv{
			Workspace:  project.Dir,
			Bins:       s.Bins,
			OnMutation: timelineTx.MarkMediaMutation,
			OnApplied:  func(rel string) { s.indexMedia(projectID, rel) },
		})
		tools.RegisterWeb(toolRegistry, tools.WebEnv{APIKey: s.ExaAPIKey, BaseURL: s.ExaBaseURL})
		tools.RegisterImage(toolRegistry, tools.ImageEnv{
			Workspace:  project.Dir,
			APIKey:     s.GeminiAPIKey,
			BaseURL:    s.GeminiBaseURL,
			Model:      s.GeminiImageModel,
			OnMutation: timelineTx.MarkMediaMutation,
			OnApplied:  func(rel, prompt string) { s.indexGeneratedImage(projectID, rel, prompt) },
		})
		videoClient := gemini.NewClient(s.GeminiAPIKey, s.GeminiBaseURL, s.GeminiVideoTimeout, 256<<20)
		tools.RegisterVideoGeneration(toolRegistry, tools.VideoGenerationEnv{
			Workspace:  project.Dir,
			Bins:       s.Bins,
			Client:     videoClient,
			OmniModel:  s.GeminiOmniVideoModel,
			VeoModel:   s.GeminiVeoVideoModel,
			Poll:       s.GeminiVideoPoll,
			OnMutation: timelineTx.MarkMediaMutation,
			OnApplied:  func(rel string) { s.indexMedia(projectID, rel) },
		})
		tools.RegisterAudioGeneration(toolRegistry, tools.AudioGenerationEnv{
			Workspace: project.Dir, Bins: s.Bins, Client: s.ElevenLabs, Voices: s.ElevenVoices,
			MusicClient: s.GeminiMusic, GeminiMusicModel: s.GeminiMusicModel, GeminiMusicOutputFormat: s.GeminiMusicOutputFormat,
			TTSModel: s.ElevenTTSModel, SFXModel: s.ElevenSFXModel,
			TTSOutputFormat: s.ElevenTTSOutputFormat, SFXOutputFormat: s.ElevenSFXOutputFormat,
			Limiter: s.ElevenLimiter, ProjectID: projectID, Transaction: timelineTx, Indexer: s.Indexer,
			Logger: s.Logger, OnMutation: timelineTx.MarkMediaMutation,
		})
		tools.RegisterTimeline(toolRegistry, tools.TimelineEnv{
			Transaction: timelineTx,
			Store:       s.Projects,
			ProjectID:   projectID,
			Workspace:   project.Dir,
			Bins:        s.Bins,
		})
		tools.RegisterTranscript(toolRegistry, tools.TranscriptEnv{
			Indexer:     s.Indexer,
			ProjectID:   projectID,
			Workspace:   project.Dir,
			Bins:        s.Bins,
			Transaction: timelineTx,
			OnMutation:  timelineTx.MarkMediaMutation,
			OnApplied:   func(rel string) { s.indexMedia(projectID, rel) },
		})
	}
	if toolRegistry == nil {
		writeError(w, http.StatusInternalServerError, "media tools are not configured")
		return
	}

	var sess *agent.Session
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

	provider := s.NewLLM(llmCfg)
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
		Provider: provider,
		Tools:    toolRegistry,
		MaxIters: s.MaxIters,
		Logger:   s.log(),
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
	if timelineTx != nil {
		if out.Reason == "error" || out.Reason == "canceled" || out.Reason == "max_iterations" {
			timelineTx.Rollback()
		} else if timeline, changed, commitErr := timelineTx.Commit(); commitErr != nil {
			_ = stream.Event(agent.NewEvent(agent.EventError, agent.ErrorPayload{Message: "timeline commit failed: " + commitErr.Error()}))
		} else if changed {
			_ = stream.Event(agent.NewEvent(agent.EventProjectChanged, agent.ProjectChangedPayload{
				ProjectID: projectID, Revision: timeline.Revision, TimelineChanged: true,
			}))
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
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         sess.ID,
		"updated_at": sess.UpdatedAt,
		"messages":   agent.PublicHistory(sess.Messages),
	})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	s.Sessions.Delete(r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Expected-Revision, X-Change-Summary")
		h.Set("Access-Control-Allow-Methods", "GET, PUT, POST, PATCH, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
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
