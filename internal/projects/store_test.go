package projects

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"parallax/internal/llm"
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
