package projects

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type timelineAssetRow struct {
	id  uuid.UUID
	err error
}

func (r timelineAssetRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 1 {
		return errors.New("unexpected scan destination")
	}
	id, ok := dest[0].(*uuid.UUID)
	if !ok {
		return errors.New("unexpected scan type")
	}
	*id = r.id
	return nil
}

type timelineAssetQuerier struct {
	byPath map[string]uuid.UUID
	byID   map[uuid.UUID]bool
}

func (q timelineAssetQuerier) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	if strings.Contains(query, "logical_path") {
		if id, ok := q.byPath[args[1].(string)]; ok {
			return timelineAssetRow{id: id}
		}
		return timelineAssetRow{err: pgx.ErrNoRows}
	}
	id := args[1].(uuid.UUID)
	if q.byID[id] {
		return timelineAssetRow{id: id}
	}
	return timelineAssetRow{err: pgx.ErrNoRows}
}

func TestResolvePGTimelineAssetsIncludesCurrentWorkspaceImport(t *testing.T) {
	generatedID := uuid.New()
	stableID := uuid.New()
	doc := Timeline{Clips: []TimelineClip{
		{ID: "generated", MediaPath: "media/generated.mp4"},
		{ID: "existing", AssetID: stableID.String(), MediaPath: "media/existing.mp4"},
		{ID: "title", Kind: "title"},
	}}
	q := timelineAssetQuerier{
		byPath: map[string]uuid.UUID{"media/generated.mp4": generatedID},
		byID:   map[uuid.UUID]bool{stableID: true},
	}

	if err := resolvePGTimelineAssets(context.Background(), q, uuid.NewString(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Clips[0].AssetID != generatedID.String() {
		t.Fatalf("generated asset id = %q, want %q", doc.Clips[0].AssetID, generatedID)
	}
	if doc.Clips[1].AssetID != stableID.String() {
		t.Fatalf("stable asset id = %q, want %q", doc.Clips[1].AssetID, stableID)
	}
}

func TestResolvePGTimelineAssetsRejectsUnknownOrMalformedAssets(t *testing.T) {
	for _, clip := range []TimelineClip{
		{ID: "missing-path", MediaPath: "media/missing.mp4"},
		{ID: "missing-id", AssetID: uuid.NewString()},
		{ID: "malformed-id", AssetID: "not-a-uuid"},
	} {
		doc := Timeline{Clips: []TimelineClip{clip}}
		err := resolvePGTimelineAssets(context.Background(), timelineAssetQuerier{}, uuid.NewString(), &doc)
		if !errors.Is(err, ErrInvalidTimeline) {
			t.Fatalf("clip %q error = %v, want ErrInvalidTimeline", clip.ID, err)
		}
	}
}
