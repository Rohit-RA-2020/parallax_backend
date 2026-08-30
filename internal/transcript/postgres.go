package transcript

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

func millis(seconds float64) int64 {
	if seconds <= 0 {
		return 0
	}
	return int64(math.Round(seconds * 1000))
}

func (x *Indexer) assetVersionID(ctx context.Context, projectID, rel string) (uuid.UUID, bool) {
	if x == nil || x.Database == nil || x.Database.Pool == nil {
		return uuid.Nil, false
	}
	var id uuid.UUID
	err := x.Database.Pool.QueryRow(ctx, `
		SELECT a.current_version_id FROM assets a
		WHERE a.project_id=$1 AND a.logical_path=$2 AND a.deleted_at IS NULL`,
		projectID, filepath.ToSlash(strings.TrimSpace(rel))).Scan(&id)
	return id, err == nil && id != uuid.Nil
}

func (x *Indexer) persistJobStatus(projectID string, st JobStatus) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	versionID, ok := x.assetVersionID(ctx, projectID, st.Path)
	if !ok {
		return
	}
	state := "running"
	switch st.State {
	case StateQueued:
		state = "queued"
	case StateReady, StateSkipped:
		state = "ready"
	case StateFailed, StateIndexFailed:
		state = "failed"
	}
	progress, _ := json.Marshal(map[string]any{"label": st.Progress, "at": st.At, "duration": st.Duration, "source_state": st.State})
	timings, _ := json.Marshal(st.Timings)
	key := "index:" + versionID.String()
	id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(projectID+":"+key))
	_, err := x.Database.Pool.Exec(ctx, `
		INSERT INTO jobs(id,project_id,asset_version_id,job_type,state,progress,timings,error,idempotency_key,attempts,lease_owner,lease_expires_at,completed_at)
		VALUES($1,$2,$3,'analysis',$4,$5,$6,$7,$8,1,CASE WHEN $4='running' THEN 'backend' ELSE '' END,CASE WHEN $4='running' THEN now()+interval '5 minutes' END,CASE WHEN $4 IN ('ready','failed') THEN now() END)
		ON CONFLICT(project_id,idempotency_key) DO UPDATE SET
		state=excluded.state,progress=excluded.progress,timings=excluded.timings,error=excluded.error,
		lease_owner=excluded.lease_owner,lease_expires_at=excluded.lease_expires_at,updated_at=now(),completed_at=excluded.completed_at`,
		id, projectID, versionID, state, progress, timings, st.Error, key)
	if err != nil {
		x.log().Error("persist analysis job", "project", projectID, "path", st.Path, "err", err)
	}
}

func (x *Indexer) persistTranscript(ctx context.Context, projectID string, doc *Document) error {
	if doc == nil {
		return nil
	}
	versionID, ok := x.assetVersionID(ctx, projectID, doc.Path)
	if !ok {
		return nil
	}
	tx, err := x.Database.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	transcriptID := uuid.NewSHA1(uuid.NameSpaceURL, []byte("transcript:"+versionID.String()))
	words, _ := json.Marshal(doc.Words)
	var embedded any
	if doc.Embedded {
		embedded = time.Now().UTC()
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO transcripts(id,asset_version_id,content_hash,audio_hash,language,duration_ms,asr_model,words,embedded_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT(asset_version_id) DO UPDATE SET content_hash=excluded.content_hash,audio_hash=excluded.audio_hash,
		language=excluded.language,duration_ms=excluded.duration_ms,asr_model=excluded.asr_model,words=excluded.words,
		embedded_at=excluded.embedded_at,updated_at=now()`, transcriptID, versionID, doc.ContentHash, doc.AudioHash,
		doc.Language, millis(doc.Duration), doc.ASRModel, words, embedded)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM transcript_segments WHERE transcript_id=$1`, transcriptID); err != nil {
		return err
	}
	for i, segment := range doc.Segments {
		pointID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(doc.ContentHash+":"+segment.ID))
		segmentID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(transcriptID.String()+":"+segment.ID))
		if _, err = tx.Exec(ctx, `INSERT INTO transcript_segments(id,transcript_id,ordinal,start_ms,end_ms,text,text_en,qdrant_point_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, segmentID, transcriptID, i, millis(segment.Start), millis(segment.End), segment.Text, segment.TextEN, pointID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (x *Indexer) persistImageCaption(ctx context.Context, projectID string, doc *ImageCaption) error {
	if doc == nil {
		return nil
	}
	versionID, ok := x.assetVersionID(ctx, projectID, doc.Path)
	if !ok {
		return nil
	}
	pointID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(doc.ContentHash+":"+imagePointSegment))
	var embedded any
	if doc.Embedded {
		embedded = time.Now().UTC()
	}
	_, err := x.Database.Pool.Exec(ctx, `INSERT INTO image_captions(asset_version_id,text_en,prompt,width,height,model,qdrant_point_id,embedded_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(asset_version_id) DO UPDATE SET text_en=excluded.text_en,prompt=excluded.prompt,
		width=excluded.width,height=excluded.height,model=excluded.model,qdrant_point_id=excluded.qdrant_point_id,embedded_at=excluded.embedded_at,updated_at=now()`,
		versionID, doc.TextEN, doc.Prompt, doc.Width, doc.Height, doc.Model, pointID, embedded)
	return err
}

func (x *Indexer) persistVideoScenes(ctx context.Context, projectID string, doc *VideoScenes) error {
	if doc == nil {
		return nil
	}
	versionID, ok := x.assetVersionID(ctx, projectID, doc.Path)
	if !ok {
		return nil
	}
	tx, err := x.Database.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `DELETE FROM video_scenes WHERE asset_version_id=$1`, versionID); err != nil {
		return err
	}
	for i, scene := range doc.Scenes {
		id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(versionID.String()+":"+scene.ID))
		pointID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(doc.ContentHash+":"+scene.ID))
		if _, err = tx.Exec(ctx, `INSERT INTO video_scenes(id,asset_version_id,ordinal,start_ms,end_ms,at_ms,text_en,spoken_en,qdrant_point_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, versionID, i, millis(scene.Start), millis(scene.End), millis(scene.At), scene.TextEN, scene.SpokenEN, pointID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (x *Indexer) persistGeneratedAudio(ctx context.Context, projectID, rel string, metadata GeneratedAudioMetadata) error {
	versionID, ok := x.assetVersionID(ctx, projectID, rel)
	if !ok {
		return nil
	}
	body, _ := json.Marshal(metadata)
	_, err := x.Database.Pool.Exec(ctx, `INSERT INTO generated_audio_metadata(asset_version_id,metadata) VALUES($1,$2)
		ON CONFLICT(asset_version_id) DO UPDATE SET metadata=excluded.metadata,updated_at=now()`, versionID, body)
	return err
}
