-- +goose Up
CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE users (
    id uuid PRIMARY KEY,
    email citext NOT NULL UNIQUE,
    display_name text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE user_preferences (
    user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    active_llm_profile_id text NOT NULL DEFAULT '',
    thinking_effort text NOT NULL DEFAULT 'medium' CHECK (thinking_effort IN ('none','low','medium','high')),
    preferences jsonb NOT NULL DEFAULT '{}'::jsonb,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE projects (
    id uuid PRIMARY KEY,
    owner_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 120),
    state text NOT NULL DEFAULT 'active' CHECK (state IN ('active','deleting')),
    head_revision_no bigint NOT NULL DEFAULT 0 CHECK (head_revision_no >= 0),
    fps integer NOT NULL DEFAULT 24 CHECK (fps BETWEEN 1 AND 240),
    canvas_width integer NOT NULL DEFAULT 1920 CHECK (canvas_width > 0),
    canvas_height integer NOT NULL DEFAULT 1080 CHECK (canvas_height > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX projects_owner_updated_idx ON projects(owner_id, updated_at DESC) WHERE state = 'active';

CREATE TABLE storage_objects (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    object_key text NOT NULL UNIQUE,
    sha256 text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    byte_size bigint NOT NULL CHECK (byte_size >= 0),
    mime_type text NOT NULL DEFAULT 'application/octet-stream',
    etag text NOT NULL DEFAULT '',
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','ready','deleting','failed')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(project_id, sha256)
);
CREATE INDEX storage_objects_project_state_idx ON storage_objects(project_id, state);

CREATE TABLE assets (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    current_version_id uuid,
    display_name text NOT NULL,
    logical_path text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('video','audio','image','subtitle','file','export')),
    origin text NOT NULL DEFAULT '',
    created_by uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz
);
CREATE UNIQUE INDEX assets_live_path_idx ON assets(project_id, logical_path) WHERE deleted_at IS NULL;
CREATE INDEX assets_project_updated_idx ON assets(project_id, updated_at DESC) WHERE deleted_at IS NULL;

CREATE TABLE asset_versions (
    id uuid PRIMARY KEY,
    asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    version_no bigint NOT NULL CHECK (version_no > 0),
    storage_object_id uuid NOT NULL REFERENCES storage_objects(id) DEFERRABLE INITIALLY DEFERRED,
    probe jsonb NOT NULL DEFAULT '{}'::jsonb,
    generation jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_by uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(asset_id, version_no)
);
ALTER TABLE assets ADD CONSTRAINT assets_current_version_fk
    FOREIGN KEY (current_version_id) REFERENCES asset_versions(id) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE asset_derivatives (
    id uuid PRIMARY KEY,
    asset_version_id uuid NOT NULL REFERENCES asset_versions(id) ON DELETE CASCADE,
    role text NOT NULL CHECK (role IN ('preview','poster','filmstrip','waveform','caption','review_frame')),
    variant_key text NOT NULL DEFAULT '',
    storage_object_id uuid NOT NULL REFERENCES storage_objects(id) DEFERRABLE INITIALLY DEFERRED,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(asset_version_id, role, variant_key)
);

CREATE TABLE chats (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    created_by uuid REFERENCES users(id) ON DELETE SET NULL,
    title text NOT NULL DEFAULT 'New chat' CHECK (length(title) <= 80),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX chats_project_updated_idx ON chats(project_id, updated_at DESC);

CREATE TABLE project_revisions (
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    revision_no bigint NOT NULL CHECK (revision_no >= 0),
    parent_revision_no bigint,
    actor_type text NOT NULL CHECK (actor_type IN ('human','agent','system')),
    actor_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
    summary text NOT NULL CHECK (length(summary) <= 240),
    chat_id uuid REFERENCES chats(id) ON DELETE SET NULL,
    timeline jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(project_id, revision_no),
    FOREIGN KEY(project_id, parent_revision_no) REFERENCES project_revisions(project_id, revision_no) DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE revision_assets (
    project_id uuid NOT NULL,
    revision_no bigint NOT NULL,
    asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    asset_version_id uuid NOT NULL REFERENCES asset_versions(id) ON DELETE RESTRICT,
    logical_path text NOT NULL,
    display_name text NOT NULL,
    present boolean NOT NULL DEFAULT true,
    PRIMARY KEY(project_id, revision_no, asset_id),
    FOREIGN KEY(project_id, revision_no) REFERENCES project_revisions(project_id, revision_no) ON DELETE CASCADE
);

CREATE TABLE checkpoints (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL,
    revision_no bigint NOT NULL,
    name citext NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 80),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(project_id, name),
    FOREIGN KEY(project_id, revision_no) REFERENCES project_revisions(project_id, revision_no) ON DELETE CASCADE
);

CREATE TABLE chat_messages (
    id uuid PRIMARY KEY,
    chat_id uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    sequence_no bigint NOT NULL CHECK (sequence_no >= 0),
    role text NOT NULL CHECK (role IN ('system','user','assistant','tool')),
    message jsonb NOT NULL,
    response_duration_ms bigint CHECK (response_duration_ms >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(chat_id, sequence_no)
);

CREATE TABLE chat_attachments (
    id uuid PRIMARY KEY,
    message_id uuid NOT NULL REFERENCES chat_messages(id) ON DELETE CASCADE,
    storage_object_id uuid NOT NULL REFERENCES storage_objects(id) DEFERRABLE INITIALLY DEFERRED,
    name text NOT NULL DEFAULT '',
    mime_type text NOT NULL DEFAULT 'application/octet-stream',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE agent_runs (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    chat_id uuid REFERENCES chats(id) ON DELETE SET NULL,
    user_id uuid REFERENCES users(id) ON DELETE SET NULL,
    model_profile_id text NOT NULL DEFAULT '',
    thinking_effort text NOT NULL DEFAULT 'medium' CHECK (thinking_effort IN ('none','low','medium','high')),
    state text NOT NULL CHECK (state IN ('running','completed','failed','cancelled')),
    error text NOT NULL DEFAULT '',
    committed_revision_no bigint,
    started_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz
);

CREATE TABLE agent_events (
    run_id uuid NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    sequence_no bigint NOT NULL CHECK (sequence_no >= 0),
    event_type text NOT NULL,
    payload jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(run_id, sequence_no)
);

CREATE TABLE visual_reviews (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL,
    revision_no bigint NOT NULL,
    mode text NOT NULL CHECK (mode IN ('changed','full')),
    state text NOT NULL CHECK (state IN ('ready','degraded','failed')),
    error text NOT NULL DEFAULT '',
    result jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(project_id, revision_no, mode),
    FOREIGN KEY(project_id, revision_no) REFERENCES project_revisions(project_id, revision_no) ON DELETE CASCADE
);

CREATE TABLE jobs (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    asset_version_id uuid REFERENCES asset_versions(id) ON DELETE CASCADE,
    job_type text NOT NULL,
    state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','running','ready','failed','cancelled')),
    progress jsonb NOT NULL DEFAULT '{}'::jsonb,
    timings jsonb NOT NULL DEFAULT '{}'::jsonb,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    error text NOT NULL DEFAULT '',
    idempotency_key text NOT NULL,
    lease_owner text NOT NULL DEFAULT '',
    lease_expires_at timestamptz,
    next_run_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    UNIQUE(project_id, idempotency_key)
);
CREATE INDEX jobs_claim_idx ON jobs(state, next_run_at, created_at) WHERE state IN ('queued','running');

CREATE TABLE transcripts (
    id uuid PRIMARY KEY,
    asset_version_id uuid NOT NULL UNIQUE REFERENCES asset_versions(id) ON DELETE CASCADE,
    content_hash text NOT NULL,
    audio_hash text NOT NULL DEFAULT '',
    language text NOT NULL DEFAULT '',
    duration_ms bigint NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
    asr_model text NOT NULL DEFAULT '',
    device text NOT NULL DEFAULT '',
    words jsonb NOT NULL DEFAULT '[]'::jsonb,
    embedded_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE transcript_segments (
    id uuid PRIMARY KEY,
    transcript_id uuid NOT NULL REFERENCES transcripts(id) ON DELETE CASCADE,
    ordinal integer NOT NULL CHECK (ordinal >= 0),
    start_ms bigint NOT NULL CHECK (start_ms >= 0),
    end_ms bigint NOT NULL CHECK (end_ms >= start_ms),
    text text NOT NULL DEFAULT '',
    text_en text NOT NULL DEFAULT '',
    qdrant_point_id uuid,
    UNIQUE(transcript_id, ordinal)
);
CREATE INDEX transcript_segments_time_idx ON transcript_segments(transcript_id, start_ms, end_ms);

CREATE TABLE image_captions (
    asset_version_id uuid PRIMARY KEY REFERENCES asset_versions(id) ON DELETE CASCADE,
    text_en text NOT NULL,
    prompt text NOT NULL DEFAULT '',
    width integer NOT NULL DEFAULT 0,
    height integer NOT NULL DEFAULT 0,
    model text NOT NULL DEFAULT '',
    qdrant_point_id uuid,
    embedded_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE video_scenes (
    id uuid PRIMARY KEY,
    asset_version_id uuid NOT NULL REFERENCES asset_versions(id) ON DELETE CASCADE,
    ordinal integer NOT NULL CHECK (ordinal >= 0),
    start_ms bigint NOT NULL CHECK (start_ms >= 0),
    end_ms bigint NOT NULL CHECK (end_ms >= start_ms),
    at_ms bigint NOT NULL CHECK (at_ms >= 0),
    text_en text NOT NULL,
    spoken_en text NOT NULL DEFAULT '',
    qdrant_point_id uuid,
    UNIQUE(asset_version_id, ordinal)
);
CREATE INDEX video_scenes_time_idx ON video_scenes(asset_version_id, start_ms, end_ms);

CREATE TABLE generated_audio_metadata (
    asset_version_id uuid PRIMARY KEY REFERENCES asset_versions(id) ON DELETE CASCADE,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE upload_sessions (
    id uuid PRIMARY KEY,
    owner_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    tus_id text NOT NULL UNIQUE,
    original_name text NOT NULL,
    mime_type text NOT NULL DEFAULT 'application/octet-stream',
    expected_size bigint NOT NULL CHECK (expected_size >= 0),
    current_offset bigint NOT NULL DEFAULT 0 CHECK (current_offset >= 0),
    temp_identifier text NOT NULL,
    state text NOT NULL DEFAULT 'receiving' CHECK (state IN ('receiving','finalizing','ready','failed','expired','cancelled')),
    resulting_asset_id uuid REFERENCES assets(id) ON DELETE SET NULL,
    attempts integer NOT NULL DEFAULT 0,
    error text NOT NULL DEFAULT '',
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX upload_sessions_owner_state_idx ON upload_sessions(owner_id, state, updated_at DESC);
CREATE INDEX upload_sessions_expiry_idx ON upload_sessions(expires_at) WHERE state IN ('receiving','failed','expired');

-- +goose Down
DROP TABLE IF EXISTS upload_sessions, generated_audio_metadata, video_scenes, image_captions,
    transcript_segments, transcripts, jobs, visual_reviews, agent_events, agent_runs,
    chat_attachments, chat_messages, checkpoints, revision_assets, project_revisions,
    chats, asset_derivatives, asset_versions, assets, storage_objects, projects,
    user_preferences, users CASCADE;
