package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"parallax/internal/projects"
)

func (s *Server) handleSearchGIFs(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	response, err := s.GIFs.SearchPage(r.Context(), query, limit, offset)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleImportGIF(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, err := s.Projects.Get(projectID); err != nil {
		writeProjectError(w, err)
		return
	}
	var body struct {
		ImportRef string `json:"import_ref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(body.ImportRef) == "" {
		writeError(w, http.StatusBadRequest, "import_ref is required")
		return
	}
	download, err := s.GIFs.Fetch(r.Context(), body.ImportRef)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer download.Body.Close()
	media, err := s.Projects.SaveUpload(projectID, download.Name, download.Body)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	if err := s.Projects.SetMediaOrigin(projectID, media.Path, "gif"); err != nil {
		s.log().Error("mark GIF import", "project", projectID, "path", media.Path, "err", err)
	}
	media.Origin = "gif"
	history, err := s.Projects.History(projectID)
	if err == nil {
		_, err = s.Projects.CommitMediaState(projectID, history.Head, projects.CommitMeta{Actor: "human", Summary: "Imported GIF"})
	}
	if err != nil {
		s.log().Error("commit GIF import", "project", projectID, "path", media.Path, "err", err)
	}
	s.indexMedia(projectID, media.Path)
	items := s.mediaResponses(projectID, []projects.Media{media})
	writeJSON(w, http.StatusCreated, items[0])
}
