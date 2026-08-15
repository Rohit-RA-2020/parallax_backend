package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"parallax/internal/agent"
	"parallax/internal/config"
	"parallax/internal/ffmpeg"
	"parallax/internal/llm"
	"parallax/internal/projects"
	"parallax/internal/tools"
)

// ProviderFactory builds a ChatProvider from the current LLM settings.
// Tests inject a fake; production uses the OpenAI-compatible HTTP client.
type ProviderFactory func(cfg config.LLM) llm.ChatProvider

type Server struct {
	Addr      string
	Settings  *config.Store
	Sessions  *agent.Store
	Tools     *tools.Registry
	Bins      ffmpeg.Bins
	Projects  *projects.Store
	NewLLM    ProviderFactory
	MaxIters  int
	Logger    *slog.Logger
	Workspace string
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
		mux.HandleFunc("GET /v1/projects/{id}/media", s.handleListMedia)
		mux.HandleFunc("POST /v1/projects/{id}/media", s.handleUploadMedia)
		mux.HandleFunc("GET /v1/projects/{id}/files/{path...}", s.handleProjectFile)
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
	writeJSON(w, http.StatusOK, s.Settings.Get().Masked())
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var body config.LLM
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	updated, err := s.Settings.Update(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated.Masked())
}

type chatRequest struct {
	SessionID string        `json:"session_id"`
	ProjectID string        `json:"project_id"`
	Message   string        `json:"message"`
	Messages  []llm.Message `json:"messages"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	userText := strings.TrimSpace(req.Message)
	if userText == "" && len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "message is required")
		return
	}

	llmCfg := s.Settings.Get()
	if err := config.ValidateLLM(llmCfg); err != nil {
		writeError(w, http.StatusFailedDependency, "LLM is not configured: "+err.Error())
		return
	}

	toolRegistry := s.Tools
	if strings.TrimSpace(req.ProjectID) != "" {
		if s.Projects == nil {
			writeError(w, http.StatusBadRequest, "projects are not configured")
			return
		}
		project, err := s.Projects.Get(req.ProjectID)
		if err != nil {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		toolRegistry = tools.NewRegistry()
		tools.RegisterMedia(toolRegistry, tools.MediaEnv{Workspace: project.Dir, Bins: s.Bins})
	}
	if toolRegistry == nil {
		writeError(w, http.StatusInternalServerError, "media tools are not configured")
		return
	}

	sess := s.Sessions.GetOrCreateForProject(req.SessionID, strings.TrimSpace(req.ProjectID))
	msgs := append([]llm.Message(nil), sess.Messages...)
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
	if userText != "" {
		lastUser := false
		if n := len(msgs); n > 0 && msgs[n-1].Role == llm.RoleUser && msgs[n-1].Content == userText {
			lastUser = true
		}
		if !lastUser {
			msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: userText})
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

	ag := &agent.Agent{
		Provider: provider,
		Tools:    toolRegistry,
		MaxIters: s.MaxIters,
		Logger:   s.log(),
	}
	out := ag.Run(r.Context(), agent.Input{
		SessionID: sess.ID,
		Messages:  msgs,
	}, func(ev agent.Event) {
		_ = stream.Event(ev)
	})
	s.Sessions.ReplaceMessages(sess.ID, out.Messages)
	if strings.TrimSpace(req.ProjectID) != "" {
		_ = s.Projects.Touch(req.ProjectID)
	}
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
		h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		h.Set("Access-Control-Allow-Methods", "GET, PUT, POST, DELETE, OPTIONS")
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
