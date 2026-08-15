package projects

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
