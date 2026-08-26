package projects

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMediaOriginPersistsAcrossStoreReload(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.Create("GIF")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project.Dir, "media", "reaction-gif.mp4")
	if err := os.WriteFile(path, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMediaOrigin(project.ID, "media/reaction-gif.mp4", "gif"); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	media, err := reloaded.ListMedia(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(media) != 1 || media[0].Origin != "gif" {
		t.Fatalf("media = %#v", media)
	}
}

func TestLooksLikeGIFImport(t *testing.T) {
	for _, path := range []string{"media/reaction.gif", "media/reaction-gif.mp4", "media/reaction.gif.webm"} {
		if !LooksLikeGIFImport(path) {
			t.Fatalf("%q should be recognized as a GIF import", path)
		}
	}
	if LooksLikeGIFImport("media/interview.mp4") {
		t.Fatal("ordinary video recognized as GIF")
	}
}
