package llm

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestEncodeChatMessagesIncludesExactAttachmentHandleAndPixels(t *testing.T) {
	raw := []byte{0x89, 0x50, 0x4e, 0x47, 0x00, 0xff}
	wire := EncodeChatMessages([]Message{{
		Role:    RoleUser,
		Content: "remove the background",
		Images:  []ImageRef{{Path: "media/reference.png", MIME: "image/png", Data: base64.StdEncoding.EncodeToString(raw)}},
	}})
	if len(wire) != 1 {
		t.Fatalf("wire messages=%d", len(wire))
	}
	var parts []map[string]any
	if err := json.Unmarshal(wire[0].Content, &parts); err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 {
		t.Fatalf("parts=%#v", parts)
	}
	if parts[1]["type"] != "text" || !strings.Contains(parts[1]["text"].(string), "media/reference.png") {
		t.Fatalf("attachment handle=%#v", parts[1])
	}
	imageURL := parts[2]["image_url"].(map[string]any)["url"].(string)
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
	if imageURL != want {
		t.Fatalf("image url=%q want=%q", imageURL, want)
	}
}
