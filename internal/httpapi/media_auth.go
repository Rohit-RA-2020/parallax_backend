package httpapi

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"parallax/internal/auth"
)

func (s *Server) handleMediaSession(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil {
		writeError(w, http.StatusServiceUnavailable, "authentication is not configured")
		return
	}
	user, ok := auth.UserFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	expires := s.Auth.SetMediaCookie(w, user)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "expires_at": expires})
}

func (s *Server) handleClearMediaSession(w http.ResponseWriter, _ *http.Request) {
	if s.Auth != nil {
		s.Auth.ClearMediaCookie(w)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMediaContent(w http.ResponseWriter, r *http.Request) {
	user, err := s.Auth.AuthenticateMedia(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	var key, mimeType, name, etag string
	var size int64
	err = s.Database.Pool.QueryRow(r.Context(), `
		SELECT o.object_key,o.mime_type,a.display_name,o.etag,o.byte_size
		FROM asset_versions v
		JOIN assets a ON a.id=v.asset_id
		JOIN projects p ON p.id=a.project_id
		JOIN storage_objects o ON o.id=v.storage_object_id
		WHERE v.id=$1 AND p.owner_id=$2 AND p.state='active' AND o.state='ready'`,
		r.PathValue("versionId"), user.ID).Scan(&key, &mimeType, &name, &etag, &size)
	if err != nil {
		writeError(w, http.StatusNotFound, "media not found")
		return
	}
	setMediaHeaders(w.Header(), mimeType, name, etag)
	s.serveLocalObject(w, r, key, name)
}

func (s *Server) handleMediaObject(w http.ResponseWriter, r *http.Request) {
	user, err := s.Auth.AuthenticateMedia(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	var key, mimeType, etag string
	var size int64
	err = s.Database.Pool.QueryRow(r.Context(), `SELECT o.object_key,o.mime_type,o.etag,o.byte_size FROM storage_objects o JOIN projects p ON p.id=o.project_id WHERE o.id=$1 AND p.owner_id=$2 AND p.state='active' AND o.state='ready'`, r.PathValue("objectId"), user.ID).Scan(&key, &mimeType, &etag, &size)
	if err != nil {
		writeError(w, http.StatusNotFound, "media not found")
		return
	}
	s.proxyMediaObject(w, r, key, mimeType, "preview", etag, size)
}

func (s *Server) proxyMediaObject(w http.ResponseWriter, r *http.Request, key, mimeType, name, etag string, size int64) {
	setMediaHeaders(w.Header(), mimeType, name, etag)
	_ = size
	s.serveLocalObject(w, r, key, name)
}

func (s *Server) serveLocalObject(w http.ResponseWriter, r *http.Request, key, name string) {
	path, err := s.Objects.Path(key)
	if err != nil {
		writeError(w, http.StatusNotFound, "media not found")
		return
	}
	file, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "media not found")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "media not found")
		return
	}
	http.ServeContent(w, r, name, info.ModTime(), file)
}

func setMediaHeaders(h http.Header, mimeType, name, etag string) {
	h.Set("Accept-Ranges", "bytes")
	h.Set("Cache-Control", "private, no-store")
	h.Set("Content-Type", mimeType)
	h.Set("Content-Disposition", fmt.Sprintf(`inline; filename="%s"`, strings.ReplaceAll(name, `"`, "")))
	if etag != "" {
		h.Set("ETag", `"`+strings.Trim(etag, `"`)+`"`)
	}
}
