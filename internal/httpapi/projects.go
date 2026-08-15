package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"parallax/internal/projects"
)

const maxUploadBytes = 2 << 30

type createProjectRequest struct {
	Name string `json:"name"`
}

type projectResponse struct {
	projects.Project
	MediaCount int `json:"media_count"`
}

type mediaResponse struct {
	projects.Media
	ContentURL string `json:"content_url"`
}

func (s *Server) handleListProjects(w http.ResponseWriter, _ *http.Request) {
	items := s.Projects.List()
	out := make([]projectResponse, 0, len(items))
	for _, p := range items {
		media, _ := s.Projects.ListMedia(p.ID)
		out = append(out, projectResponse{Project: p, MediaCount: len(media)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": out})
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var body createProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	p, err := s.Projects.Create(body.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, projectResponse{Project: p, MediaCount: 0})
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.Projects.Get(r.PathValue("id"))
	if err != nil {
		writeProjectError(w, err)
		return
	}
	media, err := s.Projects.ListMedia(p.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project": projectResponse{Project: p, MediaCount: len(media)},
		"media":   mediaResponses(p.ID, media),
	})
}

func (s *Server) handleListMedia(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	media, err := s.Projects.ListMedia(id)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"media": mediaResponses(id, media)})
}

func (s *Server) handleUploadMedia(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Projects.Get(id); err != nil {
		writeProjectError(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "expected multipart form data")
		return
	}
	var uploaded []projects.Media
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if part.FileName() == "" {
			_ = part.Close()
			continue
		}
		media, saveErr := s.Projects.SaveUpload(id, part.FileName(), part)
		_ = part.Close()
		if saveErr != nil {
			writeError(w, http.StatusBadRequest, saveErr.Error())
			return
		}
		uploaded = append(uploaded, media)
	}
	if len(uploaded) == 0 {
		writeError(w, http.StatusBadRequest, "no media files were uploaded")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"media": mediaResponses(id, uploaded)})
}

func (s *Server) handleProjectFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rel := r.PathValue("path")
	full, err := s.Projects.ResolveFile(id, rel)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	f, err := os.Open(full)
	if err != nil {
		writeError(w, http.StatusNotFound, "media not found")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Disposition", `inline; filename="`+strings.ReplaceAll(filepath.Base(full), `"`, "")+`"`)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

func mediaResponses(projectID string, media []projects.Media) []mediaResponse {
	out := make([]mediaResponse, 0, len(media))
	for _, item := range media {
		out = append(out, mediaResponse{Media: item, ContentURL: projectFileURL(projectID, item.Path)})
	}
	return out
}

func projectFileURL(projectID, path string) string {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return "/v1/projects/" + url.PathEscape(projectID) + "/files/" + strings.Join(parts, "/")
}

func writeProjectError(w http.ResponseWriter, err error) {
	if errors.Is(err, projects.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "project or media not found")
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}
