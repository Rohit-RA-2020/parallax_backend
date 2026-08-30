package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CommitDerivative stores a generated preview artifact durably and associates it
// with the exact immutable source asset version.
func (s *Store) CommitDerivative(ctx context.Context, projectID, assetPath, role, variant, source string, metadata any) (string, error) {
	if s == nil || s.pg == nil || s.objects == nil {
		return "", errors.New("durable derivatives require PostgreSQL and media storage")
	}
	var versionID uuid.UUID
	err := s.pg.QueryRow(ctx, `SELECT current_version_id FROM assets WHERE project_id=$1 AND logical_path=$2 AND deleted_at IS NULL`, projectID, filepath.ToSlash(assetPath)).Scan(&versionID)
	if err != nil {
		return "", err
	}
	mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(source)))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	object, err := s.objects.UploadFile(ctx, projectID, source, mimeType)
	if err != nil {
		return "", err
	}
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var objectID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM storage_objects WHERE project_id=$1 AND sha256=$2 AND state='ready'`, projectID, object.SHA256).Scan(&objectID)
	if errors.Is(err, pgx.ErrNoRows) {
		objectID = uuid.New()
		_, err = tx.Exec(ctx, `INSERT INTO storage_objects(id,project_id,object_key,sha256,byte_size,mime_type,etag,state) VALUES($1,$2,$3,$4,$5,$6,$7,'ready')`, objectID, projectID, object.Key, object.SHA256, object.Bytes, mimeType, object.ETag)
	}
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(metadata)
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO asset_derivatives(id,asset_version_id,role,variant_key,storage_object_id,metadata) VALUES($1,$2,$3,$4,$5,$6)
		ON CONFLICT(asset_version_id,role,variant_key) DO UPDATE SET storage_object_id=excluded.storage_object_id,metadata=excluded.metadata,created_at=now()`, uuid.New(), versionID, role, variant, objectID, body); err != nil {
		return "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	return "/v1/media/objects/" + objectID.String(), nil
}

func (s *Store) DerivativeMetadata(ctx context.Context, projectID, role string) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	if s == nil || s.pg == nil {
		return out, nil
	}
	rows, err := s.pg.Query(ctx, `SELECT DISTINCT ON (a.logical_path) a.logical_path,d.metadata FROM asset_derivatives d JOIN asset_versions v ON v.id=d.asset_version_id JOIN assets a ON a.id=v.asset_id WHERE a.project_id=$1 AND a.deleted_at IS NULL AND d.role=$2 ORDER BY a.logical_path,d.created_at DESC`, projectID, role)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		var body []byte
		if err := rows.Scan(&path, &body); err != nil {
			return nil, err
		}
		out[path] = append(json.RawMessage(nil), body...)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load derivative metadata: %w", err)
	}
	return out, nil
}
