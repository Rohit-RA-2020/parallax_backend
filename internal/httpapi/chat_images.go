package httpapi

import (
	"encoding/base64"
	"fmt"
	"strings"

	"parallax/internal/llm"
)

func (s *Server) saveChatImages(projectID string, incoming []chatImageIn) ([]llm.ImageRef, error) {
	if len(incoming) > maxChatImages {
		return nil, fmt.Errorf("at most %d images can be attached", maxChatImages)
	}
	out := make([]llm.ImageRef, 0, len(incoming))
	for _, item := range incoming {
		data, mime, err := decodeChatImage(item)
		if err != nil {
			return nil, err
		}
		if len(data) > maxChatImageBytes {
			return nil, fmt.Errorf("image %s is too large (max 8MB)", firstNonEmpty(item.Name, "attachment"))
		}
		if projectID != "" && s.Projects != nil {
			saved, err := s.Projects.SaveChatImage(projectID, item.Name, mime, data)
			if err != nil {
				return nil, err
			}
			saved.Data = base64.StdEncoding.EncodeToString(data)
			out = append(out, saved)
			continue
		}
		out = append(out, llm.ImageRef{
			MIME: mime,
			Name: strings.TrimSpace(item.Name),
			Data: base64.StdEncoding.EncodeToString(data),
		})
	}
	return out, nil
}

func decodeChatImage(item chatImageIn) ([]byte, string, error) {
	raw := strings.TrimSpace(item.Data)
	if raw == "" {
		return nil, "", fmt.Errorf("image data is required")
	}
	mime := strings.TrimSpace(item.MIME)
	if i := strings.Index(raw, ","); i >= 0 && strings.Contains(strings.ToLower(raw[:i]), "base64") {
		header := raw[:i]
		raw = raw[i+1:]
		if mime == "" {
			if _, after, ok := strings.Cut(header, "data:"); ok {
				if media, _, ok := strings.Cut(after, ";"); ok {
					mime = media
				}
			}
		}
	}
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(raw)
	}
	if err != nil {
		return nil, "", fmt.Errorf("image data is not valid base64")
	}
	if !llm.LooksLikeImage(data) {
		return nil, "", fmt.Errorf("attachment is not a readable image")
	}
	if mime == "" || !strings.HasPrefix(strings.ToLower(mime), "image/") {
		mime = llm.DetectImageMIME(data)
	}
	return data, mime, nil
}

func firstNonEmpty(vals ...string) string {
	for _, val := range vals {
		if strings.TrimSpace(val) != "" {
			return strings.TrimSpace(val)
		}
	}
	return ""
}
