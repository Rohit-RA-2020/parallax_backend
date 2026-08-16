package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"parallax/internal/ffmpeg"
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
	s.attachDurations(p.ID, media)
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
	s.attachDurations(id, media)
	writeJSON(w, http.StatusOK, map[string]any{"media": mediaResponses(id, media)})
}

func (s *Server) handleUploadMedia(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Projects.Get(id); err != nil {
		writeProjectError(w, err)
		return
	}
	history, err := s.Projects.History(id)
	if err != nil {
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
	if _, err := s.Projects.CommitMediaState(id, history.Head, projects.CommitMeta{Actor: "human", Summary: "Uploaded media"}); err != nil {
		writeProjectError(w, err)
		return
	}
	s.attachDurations(id, uploaded)
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
	filename := strings.ReplaceAll(filepath.Base(full), `"`, "")
	disposition := "inline"
	if r.URL.Query().Get("download") == "1" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition", disposition+`; filename="`+filename+`"`)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

func (s *Server) handleDeleteProjectFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	history, err := s.Projects.History(id)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	timeline, err := s.Projects.GetTimeline(id)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	if err := s.Projects.DeleteFile(id, r.PathValue("path")); err != nil {
		writeProjectError(w, err)
		return
	}
	removedPath := filepath.ToSlash(filepath.Clean(filepath.FromSlash(r.PathValue("path"))))
	removedIDs := map[string]bool{}
	clips := timeline.Clips[:0]
	for _, clip := range timeline.Clips {
		if clip.MediaPath == removedPath {
			removedIDs[clip.ID] = true
		} else {
			clips = append(clips, clip)
		}
	}
	timeline.Clips = clips
	transitions := timeline.Transitions[:0]
	for _, transition := range timeline.Transitions {
		if !removedIDs[transition.FromID] && !removedIDs[transition.ToID] {
			transitions = append(transitions, transition)
		}
	}
	timeline.Transitions = transitions
	if _, err := s.Projects.CommitTimelineAndMedia(id, timeline, history.Head, projects.CommitMeta{Actor: "human", Summary: "Deleted media"}); err != nil {
		_, _ = s.Projects.RestoreRevision(id, history.Head, -1)
		writeProjectError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type createChatRequest struct {
	Title string `json:"title"`
}

type renameChatRequest struct {
	Title string `json:"title"`
}

func (s *Server) handleListChats(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	chats, err := s.Projects.ListChats(id)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": chats})
}

func (s *Server) handleCreateChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body createChatRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}
	chat, err := s.Projects.CreateChat(id, body.Title)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	_ = s.Projects.Touch(id)
	writeJSON(w, http.StatusCreated, chatResponse(chat, false))
}

func (s *Server) handleGetChat(w http.ResponseWriter, r *http.Request) {
	chat, err := s.Projects.GetChat(r.PathValue("id"), r.PathValue("chatId"))
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, chatResponse(chat, true))
}

func (s *Server) handlePatchChat(w http.ResponseWriter, r *http.Request) {
	var body renameChatRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	chat, err := s.Projects.RenameChat(r.PathValue("id"), r.PathValue("chatId"), body.Title)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	_ = s.Projects.Touch(r.PathValue("id"))
	writeJSON(w, http.StatusOK, chatResponse(chat, false))
}

func (s *Server) handleDeleteChat(w http.ResponseWriter, r *http.Request) {
	if err := s.Projects.DeleteChat(r.PathValue("id"), r.PathValue("chatId")); err != nil {
		writeProjectError(w, err)
		return
	}
	s.Sessions.Delete(r.PathValue("chatId"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetTimeline(w http.ResponseWriter, r *http.Request) {
	doc, err := s.Projects.GetTimeline(r.PathValue("id"))
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handlePutTimeline(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var doc projects.Timeline
	if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	expected := -1
	rawExpected := strings.TrimSpace(r.Header.Get("X-Expected-Revision"))
	if rawExpected == "" {
		rawExpected = strings.TrimSpace(r.URL.Query().Get("expected_revision"))
	}
	if rawExpected != "" {
		value, parseErr := strconv.Atoi(rawExpected)
		if parseErr != nil {
			writeError(w, http.StatusBadRequest, "invalid expected revision")
			return
		}
		expected = value
	}
	summary := r.Header.Get("X-Change-Summary")
	if strings.TrimSpace(summary) == "" {
		summary = r.URL.Query().Get("summary")
	}
	saved, err := s.Projects.SaveTimelineCommit(id, doc, expected, projects.CommitMeta{
		Actor: "human", Summary: summary,
	})
	if err != nil {
		writeProjectError(w, err)
		return
	}
	_ = s.Projects.Touch(id)
	writeJSON(w, http.StatusOK, saved)
}

func chatResponse(chat projects.Chat, includeMessages bool) map[string]any {
	out := map[string]any{
		"id":         chat.ID,
		"title":      chat.Title,
		"created_at": chat.CreatedAt,
		"updated_at": chat.UpdatedAt,
	}
	if includeMessages {
		out["messages"] = projects.PublicChatMessages(chat.Messages, chat.ResponseDurations, chat.ResponseTraces)
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

func mediaResponses(projectID string, media []projects.Media) []mediaResponse {
	out := make([]mediaResponse, 0, len(media))
	for _, item := range media {
		u := projectFileURL(projectID, item.Path)
		if !item.ModifiedAt.IsZero() {
			u += "?t=" + url.QueryEscape(fmt.Sprintf("%d-%d", item.ModifiedAt.UnixMilli(), item.Bytes))
		}
		out = append(out, mediaResponse{Media: item, ContentURL: u})
	}
	return out
}

func (s *Server) attachDurations(projectID string, media []projects.Media) {
	if len(media) == 0 {
		return
	}
	project, err := s.Projects.Get(projectID)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for i := range media {
		kind := media[i].Kind
		if kind != "video" && kind != "audio" && kind != "image" {
			continue
		}
		info, err := ffmpeg.ProbeMedia(ctx, s.Bins, project.Dir, media[i].Path)
		if err != nil {
			continue
		}
		if info.Duration > 0 {
			media[i].Duration = info.Duration
		}
		if info.Width > 0 && info.Height > 0 {
			media[i].Width = info.Width
			media[i].Height = info.Height
		}
	}
}

func writeProjectError(w http.ResponseWriter, err error) {
	if errors.Is(err, projects.ErrNotFound) || errors.Is(err, projects.ErrChatNotFound) || errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "project or media not found")
		return
	}
	if errors.Is(err, projects.ErrInvalidTimeline) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, projects.ErrRevisionConflict) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}
