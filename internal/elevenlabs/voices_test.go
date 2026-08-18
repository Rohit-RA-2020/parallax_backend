package elevenlabs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVoiceCatalogFiltersAndValidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voices.json")
	if err := os.WriteFile(path, []byte(`[
{"id":"warm-1","name":"Warm","description":"Calm documentary narrator","languages":["en"],"characteristics":["warm","calm"]},
{"id":"bright-1","name":"Bright","languages":["en","hi"],"characteristics":["bright"]}
]`), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := LoadVoiceCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog.List("documentary", "en", ""); len(got) != 1 || got[0].ID != "warm-1" {
		t.Fatalf("got=%+v", got)
	}
	if _, err := catalog.Get("missing"); err == nil {
		t.Fatal("expected missing voice error")
	}
}

func TestVoiceCatalogRejectsDuplicateIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voices.json")
	if err := os.WriteFile(path, []byte(`[{"id":"same","name":"One"},{"id":"same","name":"Two"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadVoiceCatalog(path); err == nil {
		t.Fatal("expected duplicate error")
	}
}
