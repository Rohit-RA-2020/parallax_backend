package httpapi

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"parallax/internal/ffmpeg"
	"parallax/internal/projects"
)

const exportTimeout = 15 * time.Minute

type exportRequest struct {
	Source     string  `json:"source"`
	Format     string  `json:"format"`
	Quality    string  `json:"quality"`
	Resolution string  `json:"resolution"`
	FPS        int     `json:"fps"`
	Audio      *bool   `json:"audio"`
	Start      float64 `json:"start"`
	Duration   float64 `json:"duration"`
	Filename   string  `json:"filename"`
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	project, err := s.Projects.Get(id)
	if err != nil {
		writeProjectError(w, err)
		return
	}

	var body exportRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	audio := true
	if body.Audio != nil {
		audio = *body.Audio
	}
	spec := ffmpeg.ExportSpec{
		Source:     body.Source,
		Format:     body.Format,
		Quality:    body.Quality,
		Resolution: body.Resolution,
		FPS:        body.FPS,
		Audio:      audio,
		Start:      body.Start,
		Duration:   body.Duration,
	}
	if err := spec.Normalize(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.Projects.ResolveFile(id, spec.Source); err != nil {
		writeProjectError(w, err)
		return
	}

	filename := strings.TrimSpace(body.Filename)
	if filename == "" {
		filename = strings.TrimSuffix(filepath.Base(spec.Source), filepath.Ext(spec.Source)) + "-export"
	}
	planned, err := s.Projects.PrepareExport(id, filename, spec.Ext())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	args, err := ffmpeg.BuildExportArgs(spec, planned.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cmd, err := ffmpeg.Validate(args, ffmpeg.ValidateOpts{Workspace: project.Dir})
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid export command: "+err.Error())
		return
	}

	res, err := ffmpeg.Run(r.Context(), s.Bins, cmd, project.Dir, exportTimeout)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	_ = res

	media, err := s.Projects.StatFile(id, planned.Path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "export finished but the file is missing")
		return
	}
	_ = s.Projects.Touch(id)

	item := mediaResponses(id, []projects.Media{media})[0]
	writeJSON(w, http.StatusCreated, map[string]any{
		"media":        item,
		"download_url": item.ContentURL + downloadQuery(item.ContentURL),
	})
}

func downloadQuery(contentURL string) string {
	if strings.Contains(contentURL, "?") {
		return "&download=1"
	}
	return "?download=1"
}
