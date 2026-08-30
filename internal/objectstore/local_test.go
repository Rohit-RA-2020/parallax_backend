package objectstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalStoreUploadDownloadAndProjectDelete(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "durable"))
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(source, []byte("durable-media"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := store.UploadFile(context.Background(), "project-1", source, "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.UploadFile(context.Background(), "project-1", source, "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	if first.Key != second.Key || first.SHA256 != second.SHA256 {
		t.Fatalf("content was not deduplicated: %#v %#v", first, second)
	}
	destination := filepath.Join(t.TempDir(), "materialized.mp4")
	if err := store.Download(context.Background(), first.Key, destination); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(destination)
	if err != nil || string(body) != "durable-media" {
		t.Fatalf("download=%q err=%v", body, err)
	}
	if err := store.DeleteProject(context.Background(), "project-1"); err != nil {
		t.Fatal(err)
	}
	path, _ := store.Path(first.Key)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("project object still exists: %v", err)
	}
}

func TestLocalStoreRejectsEscapingKeys(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"../secret", "/etc/passwd", ".."} {
		if _, err := store.Path(key); err == nil {
			t.Fatalf("accepted unsafe key %q", key)
		}
	}
}
