package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"parallax/internal/database"
	"parallax/internal/httpapi"
	"parallax/internal/objectstore"
	"parallax/internal/transcript"
)

func runPurgeWorker(ctx context.Context, db *database.DB, objects *objectstore.Client, uploads *httpapi.UploadManager, indexer *transcript.Indexer, workspace string, log *slog.Logger) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		purgeOne(ctx, db, objects, uploads, indexer, workspace, log)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func purgeOne(ctx context.Context, db *database.DB, objects *objectstore.Client, uploads *httpapi.UploadManager, indexer *transcript.Indexer, workspace string, log *slog.Logger) {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)
	var jobID, projectID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id,project_id FROM jobs WHERE job_type='project_purge' AND state IN ('queued','failed') AND next_run_at<=now() ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&jobID, &projectID)
	if err != nil {
		return
	}
	_, err = tx.Exec(ctx, `UPDATE jobs SET state='running',attempts=attempts+1,lease_owner='backend',lease_expires_at=now()+interval '5 minutes',updated_at=now() WHERE id=$1`, jobID)
	if err != nil || tx.Commit(ctx) != nil {
		return
	}

	if uploads != nil {
		err = uploads.CancelProject(projectID.String())
	}
	if err == nil && indexer != nil {
		err = indexer.RemoveProject(ctx, projectID.String())
	}
	if err == nil {
		err = objects.DeleteProject(ctx, projectID.String())
	}
	if err == nil {
		_, err = db.Pool.Exec(ctx, `DELETE FROM projects WHERE id=$1 AND state='deleting'`, projectID)
	}
	if err == nil {
		_ = os.RemoveAll(filepath.Join(workspace, "projects", projectID.String()))
		return
	}
	log.Error("project purge will retry", "project", projectID, "err", err)
	_, updateErr := db.Pool.Exec(ctx, `UPDATE jobs SET state='failed',error=$2,lease_owner='',lease_expires_at=NULL,next_run_at=now()+LEAST(interval '1 hour', interval '30 seconds' * GREATEST(attempts,1)),updated_at=now() WHERE id=$1`, jobID, err.Error())
	if updateErr != nil {
		log.Error("record project purge failure", "job", jobID, "err", fmt.Errorf("%v; update: %w", err, updateErr))
	}
}
