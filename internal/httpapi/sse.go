package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"

	"parallax/internal/agent"
)

type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func newSSE(w http.ResponseWriter) (sseWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return sseWriter{}, fmt.Errorf("streaming is not supported")
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return sseWriter{w: w, flusher: flusher}, nil
}

func (s sseWriter) Event(ev agent.Event) error {
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", ev.Type, ev.Data); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
