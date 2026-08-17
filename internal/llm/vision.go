package llm

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

const visionDetail = "high"

// EncodeChatMessages turns stored messages into the OpenAI-compatible
// chat-completions shape. Text-only content stays a string; messages with
// images become a content array of text + image_url parts.
func EncodeChatMessages(messages []Message) []wireMessage {
	clean := sanitizeMessages(messages)
	out := make([]wireMessage, 0, len(clean))
	for _, m := range clean {
		item := wireMessage{
			Role:       m.Role,
			Name:       m.Name,
			ToolCallID: m.ToolCallID,
			ToolCalls:  m.ToolCalls,
		}
		if encoded, ok := encodeVisionContent(m); ok {
			item.Content = encoded
		}
		out = append(out, item)
	}
	return out
}

func encodeVisionContent(m Message) (json.RawMessage, bool) {
	parts := make([]map[string]any, 0, 1+len(m.Images))
	if text := strings.TrimSpace(m.Content); text != "" {
		if len(m.Images) == 0 {
			raw, err := json.Marshal(m.Content)
			if err != nil {
				return nil, false
			}
			return raw, true
		}
		parts = append(parts, map[string]any{"type": "text", "text": m.Content})
	}
	for _, img := range m.Images {
		url := imageDataURL(img)
		if url == "" {
			continue
		}
		parts = append(parts, map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url":    url,
				"detail": visionDetail,
			},
		})
	}
	if len(parts) == 0 {
		if m.Role == RoleTool || m.Content != "" {
			raw, err := json.Marshal(m.Content)
			return raw, err == nil
		}
		return nil, false
	}
	raw, err := json.Marshal(parts)
	if err != nil {
		return nil, false
	}
	return raw, true
}

func imageDataURL(img ImageRef) string {
	data := strings.TrimSpace(img.Data)
	if data == "" {
		return ""
	}
	if strings.HasPrefix(data, "data:") {
		return data
	}
	mime := strings.TrimSpace(img.MIME)
	if mime == "" {
		mime = "image/jpeg"
	}
	return "data:" + mime + ";base64," + data
}

// HydrateMessageImages reads attached files into Data so EncodeChatMessages
// can emit data URLs. Missing files are left empty and skipped on the wire.
func HydrateMessageImages(messages []Message, open func(path string) ([]byte, error)) []Message {
	if open == nil {
		return messages
	}
	out := append([]Message(nil), messages...)
	for i := range out {
		if len(out[i].Images) == 0 {
			continue
		}
		images := append([]ImageRef(nil), out[i].Images...)
		for j := range images {
			if strings.TrimSpace(images[j].Data) != "" {
				continue
			}
			path := strings.TrimSpace(images[j].Path)
			if path == "" {
				continue
			}
			data, err := open(path)
			if err != nil || len(data) == 0 {
				continue
			}
			images[j].Data = base64.StdEncoding.EncodeToString(data)
			if images[j].MIME == "" {
				images[j].MIME = DetectImageMIME(data)
			}
		}
		out[i].Images = images
	}
	return out
}

func DetectImageMIME(data []byte) string {
	switch {
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "image/jpeg"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return "image/gif"
	default:
		return "image/jpeg"
	}
}

func LooksLikeImage(data []byte) bool {
	if len(data) < 8 {
		return false
	}
	mime := DetectImageMIME(data)
	return mime == "image/png" || mime == "image/jpeg" || mime == "image/webp" || mime == "image/gif"
}
