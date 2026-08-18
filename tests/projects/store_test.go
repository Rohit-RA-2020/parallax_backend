package projects_test

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"parallax/internal/llm"
	. "parallax/internal/projects"
)

func TestProjectUploadAndReload(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.Create("Launch film")
	if err != nil {
		t.Fatal(err)
	}
	media, err := store.SaveUpload(p.ID, "opening shot.mp4", strings.NewReader("video"))
	if err != nil {
		t.Fatal(err)
	}
	if media.Kind != "video" || media.Path != "media/opening shot.mp4" {
		t.Fatalf("media=%+v", media)
	}
	items, err := store.ListMedia(p.ID)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	reloaded, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reloaded.Get(p.ID); err != nil || got.Name != p.Name {
		t.Fatalf("project=%+v err=%v", got, err)
	}
	if err := reloaded.DeleteFile(p.ID, media.Path); err != nil {
		t.Fatal(err)
	}
	items, err = reloaded.ListMedia(p.ID)
	if err != nil || len(items) != 0 {
		t.Fatalf("after delete items=%+v err=%v", items, err)
	}
}

func TestListChatsEmptyIsNotNil(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.Create("Quiet")
	if err != nil {
		t.Fatal(err)
	}
	chats, err := store.ListChats(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if chats == nil {
		t.Fatal("empty chat list should be a slice")
	}
	if len(chats) != 0 {
		t.Fatalf("chats=%+v", chats)
	}
}

func TestChatsPersistAcrossReload(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.Create("Talk")
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateChat(p.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateChat(p.ID, "Color pass")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("expected distinct chats")
	}
	if _, err := store.SaveChatMessages(p.ID, first.ID, []llm.Message{
		{Role: llm.RoleUser, Content: "Mute the highway shot"},
		{Role: llm.RoleAssistant, Content: "Done. Audio is stripped."},
	}); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := reloaded.ListChats(p.ID)
	if err != nil || len(listed) != 2 {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	loaded, err := reloaded.GetChat(p.ID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Title != "Mute the highway shot" {
		t.Fatalf("title=%q", loaded.Title)
	}
	if len(loaded.Messages) != 2 || loaded.Messages[0].Content != "Mute the highway shot" {
		t.Fatalf("messages=%+v", loaded.Messages)
	}
	if err := reloaded.DeleteChat(p.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	listed, err = reloaded.ListChats(p.ID)
	if err != nil || len(listed) != 1 || listed[0].ID != first.ID {
		t.Fatalf("after delete listed=%+v err=%v", listed, err)
	}
}

func TestSaveChatMessagesDropsStaleResponseMetadata(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.Create("Retry")
	if err != nil {
		t.Fatal(err)
	}
	chat, err := store.CreateChat(p.ID, "Retry")
	if err != nil {
		t.Fatal(err)
	}
	long := []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleUser, Content: "first"},
		{Role: llm.RoleAssistant, Content: "one"},
		{Role: llm.RoleUser, Content: "second"},
		{Role: llm.RoleAssistant, Content: "two"},
	}
	if _, err := store.SaveChatMessages(p.ID, chat.ID, long); err != nil {
		t.Fatal(err)
	}
	if err := store.SetChatResponseMetadata(p.ID, chat.ID, long, 1200, []ChatTraceEvent{{Type: "done"}}); err != nil {
		t.Fatal(err)
	}
	short := long[:3]
	if _, err := store.SaveChatMessages(p.ID, chat.ID, short); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetChat(p.ID, chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 3 {
		t.Fatalf("messages=%d", len(loaded.Messages))
	}
	if _, ok := loaded.ResponseDurations["4"]; ok {
		t.Fatalf("stale duration kept: %+v", loaded.ResponseDurations)
	}
	if _, ok := loaded.ResponseTraces["4"]; ok {
		t.Fatalf("stale trace kept: %+v", loaded.ResponseTraces)
	}
}

func TestDeleteProjectRemovesWorkspace(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.Create("Cut")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveUpload(p.ID, "talk.mp4", strings.NewReader("video")); err != nil {
		t.Fatal(err)
	}
	chat, err := store.CreateChat(p.ID, "Color")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveChatMessages(p.ID, chat.ID, []llm.Message{
		{Role: llm.RoleUser, Content: "Mute the highway"},
	}); err != nil {
		t.Fatal(err)
	}
	dir := p.Dir
	if err := store.Delete(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("workspace still exists: %v", err)
	}
	reloaded, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reloaded.Get(p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reloaded get: %v", err)
	}
	if err := store.Delete(p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestResolveFileRejectsSymlink(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.Create("Safe")
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.mp4")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(p.Dir, "media", "escape.mp4")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveFile(p.ID, "media/escape.mp4"); err == nil {
		t.Fatal("symlink was served")
	}
}

func TestSaveChatImagePersistsUnderChatMedia(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.Create("Refs")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString("/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAIBAQEBAQIBAQECAgICAgQDAgICAgUEBAMEBgUGBgYFBgYGBwkIBgcJBwYGCAsICQoKCgoKBggLDAsKDAkKCgr/2wBDAQICAgICAgUDAwUKBwYHCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgr/wAARCAABAAEDASIAAhEBAxEB/8QAHwAAAQUBAQEBAQEAAAAAAAAAAAECAwQFBgcICQoL/8QAtRAAAgEDAwIEAwUFBAQAAAF9AQIDAAQRBRIhMUEGE1FhByJxFDKBkaEII0KxwRVS0fAkM2JyggkKFhcYGRolJicoKSo0NTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqDhIWGh4iJipKTlJWWl5iZmqKjpKWmp6ipqrKztLW2t7i5usLDxMXGx8jJytLT1NXW19jZ2uHi4+Tl5ufo6erx8vP09fb3+Pn6/8QAHwEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoL/8QAtREAAgECBAQDBAcFBAQAAQJ3AAECAxEEBSExBhJBUQdhcRMiMoEIFEKRobHBCSMzUvAVYnLRChYkNOEl8RcYGRomJygpKjU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6goOEhYaHiImKkpOUlZaXmJmaoqOkpaanqKmqsrO0tba3uLm6wsPExcbHyMnK0tPU1dbX2Nna4uPk5ebn6Onq8vP09fb3+Pn6/9oADAMBAAIRAxEAPwD4vooor+Uz/fw//9k=")
	if err != nil {
		t.Fatal(err)
	}
	img, err := store.SaveChatImage(p.ID, "look.png", "image/jpeg", raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(img.Path, ".parallax/chat-media/") {
		t.Fatalf("path=%s", img.Path)
	}
	if _, err := store.ResolveFile(p.ID, img.Path); err != nil {
		t.Fatal(err)
	}
	media, err := store.ListMedia(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range media {
		if item.Path == img.Path {
			t.Fatal("chat image leaked into the bin")
		}
	}
}
