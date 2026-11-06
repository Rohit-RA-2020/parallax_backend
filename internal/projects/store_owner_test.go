package projects

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestOwnerStoresShareUploadLock(t *testing.T) {
	base := &Store{storeState: &storeState{pg: &pgxpool.Pool{}, data: map[string]Project{}}}
	owner := uuid.New()
	scoped := base.ForOwner(owner)
	if scoped.ownerID != owner || base.ownerID != uuid.Nil {
		t.Fatal("owner scoping mutated the base store")
	}
	base.uploadMu.Lock()
	entered := make(chan struct{})
	go func() {
		scoped.uploadMu.Lock()
		close(entered)
		scoped.uploadMu.Unlock()
	}()
	select {
	case <-entered:
		base.uploadMu.Unlock()
		t.Fatal("owner scope bypassed the shared upload lock")
	case <-time.After(20 * time.Millisecond):
	}
	base.uploadMu.Unlock()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("owner scope retained a copied locked mutex")
	}
}
