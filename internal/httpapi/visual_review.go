package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"

	"parallax/internal/llm"
	"parallax/internal/visualreview"
)

func (s *Server) visualReviewService(provider llm.ChatProvider) *visualreview.Service {
	return &visualreview.Service{Store: s.Projects, Bins: s.Bins, Vision: provider, RenderWidth: 960, RenderHeight: 540}
}

func (s *Server) handleVisualReview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Projects.Get(id); err != nil {
		writeProjectError(w, err)
		return
	}
	var req visualreview.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.ProjectID = id
	if req.Mode == "" {
		req.Mode = visualreview.ModeFull
	}
	var provider llm.ChatProvider
	if s.NewLLM != nil && s.Settings != nil {
		provider = s.NewLLM(s.Settings.Get())
	}
	result, err := s.visualReviewService(provider).Review(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeVisualReviewJSON(w, id, result)
}

func (s *Server) handleGetVisualReview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	revision, err := strconv.Atoi(r.PathValue("revision"))
	if err != nil || revision < 0 {
		writeError(w, http.StatusBadRequest, "invalid review revision")
		return
	}
	result, err := (&visualreview.Service{Store: s.Projects}).Load(id, revision)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeVisualReviewJSON(w, id, result)
}

func writeVisualReviewJSON(w http.ResponseWriter, projectID string, result visualreview.Result) {
	for i := range result.Frames {
		result.Frames[i].Path = projectFileURL(projectID, result.Frames[i].Path)
	}
	writeJSON(w, http.StatusOK, result)
}
