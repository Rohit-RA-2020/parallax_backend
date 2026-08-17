package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"parallax/internal/projects"
)

type historyMoveRequest struct {
	ExpectedRevision int `json:"expected_revision"`
	TargetRevision   int `json:"target_revision"`
}

func (s *Server) handleGetHistory(w http.ResponseWriter, r *http.Request) {
	history, err := s.Projects.History(r.PathValue("id"))
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, publicHistory(history))
}

func decodeHistoryMove(r *http.Request) (historyMoveRequest, error) {
	var body historyMoveRequest
	if r.Body == nil || r.ContentLength == 0 {
		body.ExpectedRevision = -1
		body.TargetRevision = -1
		return body, nil
	}
	err := json.NewDecoder(r.Body).Decode(&body)
	return body, err
}

func (s *Server) handleUndoHistory(w http.ResponseWriter, r *http.Request) {
	body, err := decodeHistoryMove(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	doc, err := s.Projects.Undo(r.PathValue("id"), body.ExpectedRevision)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	_ = s.Projects.Touch(r.PathValue("id"))
	s.indexProject(r.PathValue("id"))
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handleRedoHistory(w http.ResponseWriter, r *http.Request) {
	body, err := decodeHistoryMove(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	doc, err := s.Projects.Redo(r.PathValue("id"), body.ExpectedRevision, body.TargetRevision)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	_ = s.Projects.Touch(r.PathValue("id"))
	s.indexProject(r.PathValue("id"))
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handleRestoreHistory(w http.ResponseWriter, r *http.Request) {
	body, err := decodeHistoryMove(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.TargetRevision < 0 {
		writeError(w, http.StatusBadRequest, "target_revision is required")
		return
	}
	doc, err := s.Projects.RestoreRevision(r.PathValue("id"), body.TargetRevision, body.ExpectedRevision)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	_ = s.Projects.Touch(r.PathValue("id"))
	s.indexProject(r.PathValue("id"))
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handleCreateCheckpoint(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		Revision int    `json:"revision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if err := s.Projects.CreateCheckpoint(r.PathValue("id"), body.Name, body.Revision); err != nil {
		writeProjectError(w, err)
		return
	}
	history, err := s.Projects.History(r.PathValue("id"))
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, publicHistory(history))
}

func (s *Server) handleRenameCheckpoint(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := s.Projects.RenameCheckpoint(r.PathValue("id"), r.PathValue("checkpoint"), body.Name); err != nil {
		writeProjectError(w, err)
		return
	}
	history, err := s.Projects.History(r.PathValue("id"))
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, publicHistory(history))
}

func publicHistory(history projects.History) map[string]any {
	revisions := make([]map[string]any, 0, len(history.Revisions))
	redoCandidates := history.RedoCandidates
	if redoCandidates == nil {
		redoCandidates = []int{}
	}
	for _, revision := range history.Revisions {
		children := revision.Children
		if children == nil {
			children = []int{}
		}
		checkpoints := revision.Checkpoints
		if checkpoints == nil {
			checkpoints = []string{}
		}
		revisions = append(revisions, map[string]any{"id": revision.ID, "parent_id": revision.ParentID, "actor": revision.Actor, "summary": revision.Summary, "chat_id": revision.ChatID, "created_at": revision.CreatedAt, "children": children, "checkpoints": checkpoints})
	}
	return map[string]any{"head": history.Head, "can_undo": history.CanUndo, "redo_candidates": redoCandidates, "revisions": revisions}
}

func (s *Server) handleDeleteCheckpoint(w http.ResponseWriter, r *http.Request) {
	if err := s.Projects.DeleteCheckpoint(r.PathValue("id"), r.PathValue("checkpoint")); err != nil {
		writeProjectError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
