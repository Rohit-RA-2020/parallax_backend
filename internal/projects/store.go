package projects

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"parallax/internal/llm"
	"parallax/internal/objectstore"
)

var ErrNotFound = errors.New("project not found")

type Project struct {
	ID        string    `json:"id"`
	OwnerID   string    `json:"-"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Dir       string    `json:"-"`
}

type Media struct {
	ID          string    `json:"id"`
	VersionID   string    `json:"version_id,omitempty"`
	Name        string    `json:"name"`
	Path        string    `json:"path"`
	Kind        string    `json:"kind"`
	ContentType string    `json:"content_type"`
	Bytes       int64     `json:"bytes"`
	Duration    float64   `json:"duration,omitempty"`
	Width       int       `json:"width,omitempty"`
	Height      int       `json:"height,omitempty"`
	ModifiedAt  time.Time `json:"modified_at"`
	Origin      string    `json:"origin,omitempty"`
	HasAudio    *bool     `json:"has_audio,omitempty"`
}

type Store struct {
	mu       sync.RWMutex
	uploadMu sync.Mutex
	root     string
	data     map[string]Project
	pg       *pgxpool.Pool
	objects  *objectstore.Client
	ownerID  uuid.UUID
}

// NewPostgresStore creates the production store. The root is a temporary,
// reconstructable materialization cache; PostgreSQL and local durable media storage are authoritative.
func NewPostgresStore(root string, pg *pgxpool.Pool, objects *objectstore.Client) (*Store, error) {
	if pg == nil || objects == nil {
		return nil, errors.New("postgres project store requires PostgreSQL and durable media storage")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	return &Store{root: abs, data: map[string]Project{}, pg: pg, objects: objects}, nil
}

func (s *Store) ForOwner(owner uuid.UUID) *Store {
	if s == nil || s.pg == nil {
		return s
	}
	clone := *s
	clone.ownerID = owner
	return &clone
}

func (s *Store) IsPostgres() bool { return s != nil && s.pg != nil }

func NewStore(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	s := &Store{root: abs, data: map[string]Project{}}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		p, err := readProject(filepath.Join(abs, entry.Name()))
		if err == nil {
			s.data[p.ID] = p
		}
	}
	return s, nil
}

func (s *Store) Create(name string) (Project, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Project{}, errors.New("project name is required")
	}
	if len(name) > 120 {
		return Project{}, errors.New("project name is too long")
	}
	if s.pg != nil {
		if s.ownerID == uuid.Nil {
			return Project{}, errors.New("project owner is required")
		}
		id := uuid.New()
		now := time.Now().UTC()
		p := Project{ID: id.String(), OwnerID: s.ownerID.String(), Name: name, CreatedAt: now, UpdatedAt: now, Dir: filepath.Join(s.root, id.String())}
		tx, err := s.pg.Begin(context.Background())
		if err != nil {
			return Project{}, err
		}
		defer tx.Rollback(context.Background())
		if _, err = tx.Exec(context.Background(), `INSERT INTO projects(id,owner_id,name) VALUES($1,$2,$3)`, id, s.ownerID, name); err != nil {
			return Project{}, err
		}
		timeline := emptyTimeline()
		timeline.Schema = 3
		body, _ := json.Marshal(timeline)
		if _, err = tx.Exec(context.Background(), `INSERT INTO project_revisions(project_id,revision_no,actor_type,summary,timeline) VALUES($1,0,'system','Initial project state',$2)`, id, body); err != nil {
			return Project{}, err
		}
		if err = tx.Commit(context.Background()); err != nil {
			return Project{}, err
		}
		_ = os.MkdirAll(filepath.Join(p.Dir, "media"), 0o700)
		return p, nil
	}

	now := time.Now().UTC()
	p := Project{ID: newID(), Name: name, CreatedAt: now, UpdatedAt: now}
	p.Dir = filepath.Join(s.root, p.ID)
	if err := os.MkdirAll(filepath.Join(p.Dir, "media"), 0o755); err != nil {
		return Project{}, err
	}
	if err := writeProject(p); err != nil {
		return Project{}, err
	}
	s.mu.Lock()
	s.data[p.ID] = p
	s.mu.Unlock()
	return p, nil
}

func (s *Store) List() []Project {
	if s.pg != nil {
		if s.ownerID == uuid.Nil {
			return []Project{}
		}
		rows, err := s.pg.Query(context.Background(), `SELECT id,name,created_at,updated_at FROM projects WHERE owner_id=$1 AND state='active' ORDER BY updated_at DESC`, s.ownerID)
		if err != nil {
			return []Project{}
		}
		defer rows.Close()
		out := []Project{}
		for rows.Next() {
			var p Project
			if rows.Scan(&p.ID, &p.Name, &p.CreatedAt, &p.UpdatedAt) == nil {
				p.OwnerID = s.ownerID.String()
				p.Dir = filepath.Join(s.root, p.ID)
				out = append(out, p)
			}
		}
		return out
	}
	s.mu.RLock()
	out := make([]Project, 0, len(s.data))
	for _, p := range s.data {
		out = append(out, p)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

func (s *Store) Get(id string) (Project, error) {
	if s.pg != nil {
		var p Project
		var owner uuid.UUID
		query := `SELECT id,owner_id,name,created_at,updated_at FROM projects WHERE id=$1 AND state='active'`
		args := []any{id}
		if s.ownerID != uuid.Nil {
			query += ` AND owner_id=$2`
			args = append(args, s.ownerID)
		}
		err := s.pg.QueryRow(context.Background(), query, args...).Scan(&p.ID, &owner, &p.Name, &p.CreatedAt, &p.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return Project{}, ErrNotFound
		}
		if err != nil {
			return Project{}, err
		}
		p.OwnerID = owner.String()
		p.Dir = filepath.Join(s.root, p.ID)
		return p, nil
	}
	s.mu.RLock()
	p, ok := s.data[id]
	s.mu.RUnlock()
	if !ok {
		return Project{}, ErrNotFound
	}
	return p, nil
}

func (s *Store) Touch(id string) error {
	if s.pg != nil {
		query := `UPDATE projects SET updated_at=now() WHERE id=$1 AND state='active'`
		args := []any{id}
		if s.ownerID != uuid.Nil {
			query += ` AND owner_id=$2`
			args = append(args, s.ownerID)
		}
		result, err := s.pg.Exec(context.Background(), query, args...)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.data[id]
	if !ok {
		return ErrNotFound
	}
	p.UpdatedAt = time.Now().UTC()
	if err := writeProject(p); err != nil {
		return err
	}
	s.data[id] = p
	return nil
}

func (s *Store) SaveUpload(id, originalName string, src io.Reader) (Media, error) {
	if s.pg != nil {
		if _, err := s.Get(id); err != nil {
			return Media{}, err
		}
		tmp, err := os.CreateTemp(s.root, "upload-*")
		if err != nil {
			return Media{}, err
		}
		name := tmp.Name()
		defer os.Remove(name)
		if err = copyStream(tmp, src); err != nil {
			_ = tmp.Close()
			return Media{}, err
		}
		if err = tmp.Close(); err != nil {
			return Media{}, err
		}
		return s.commitPGFile(context.Background(), id, originalName, name, "human")
	}
	p, err := s.Get(id)
	if err != nil {
		return Media{}, err
	}
	name := safeName(originalName)
	if name == "" {
		return Media{}, errors.New("uploaded file needs a valid filename")
	}
	if !isMediaName(name) {
		return Media{}, fmt.Errorf("unsupported media type %q", filepath.Ext(name))
	}
	dir := filepath.Join(p.Dir, "media")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Media{}, err
	}
	dst := availablePath(dir, name)
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return Media{}, err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := copyStream(tmp, src); err != nil {
		if IsNoSpace(err) {
			return Media{}, fmt.Errorf("not enough disk space to store this file")
		}
		return Media{}, err
	}
	if err := tmp.Sync(); err != nil {
		return Media{}, err
	}
	if err := tmp.Close(); err != nil {
		return Media{}, err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return Media{}, err
	}
	ok = true
	if err := s.Touch(id); err != nil {
		return Media{}, err
	}
	return mediaFromFile(p.Dir, dst)
}

// FinalizeUpload promotes a completed temporary upload into the project's
// content-addressed object store and links it into media/. The upload mutex
// makes filename allocation, object-index updates, and the history commit one
// atomic project-store operation from the point of view of concurrent uploads.
func (s *Store) FinalizeUpload(id, originalName, source string) (Media, error) {
	if s.pg != nil {
		return s.commitPGFile(context.Background(), id, originalName, source, "human")
	}
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()

	p, err := s.Get(id)
	if err != nil {
		return Media{}, err
	}
	name := safeName(originalName)
	if name == "" {
		return Media{}, errors.New("uploaded file needs a valid filename")
	}
	if !isMediaName(name) {
		return Media{}, fmt.Errorf("unsupported media type %q", filepath.Ext(name))
	}
	info, err := os.Stat(source)
	if err != nil {
		return Media{}, err
	}
	if !info.Mode().IsRegular() {
		return Media{}, errors.New("completed upload is not a regular file")
	}

	f, err := os.Open(source)
	if err != nil {
		return Media{}, err
	}
	h := sha256.New()
	err = copyStream(h, f)
	closeErr := f.Close()
	if err != nil {
		return Media{}, err
	}
	if closeErr != nil {
		return Media{}, closeErr
	}
	hash := hex.EncodeToString(h.Sum(nil))
	objectDir := objectsDir(p)
	if err := os.MkdirAll(objectDir, 0o700); err != nil {
		return Media{}, err
	}
	objectPath := filepath.Join(objectDir, hash)
	if _, err := os.Stat(objectPath); os.IsNotExist(err) {
		if err := os.Link(source, objectPath); err != nil {
			if err := copyFileAtomic(source, objectPath, 0o600); err != nil {
				return Media{}, err
			}
		}
	} else if err != nil {
		return Media{}, err
	}
	_ = os.Chmod(objectPath, 0o600)

	mediaDir := filepath.Join(p.Dir, "media")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		return Media{}, err
	}
	dst := availablePath(mediaDir, name)
	if err := os.Link(objectPath, dst); err != nil {
		if err := copyFileAtomic(objectPath, dst, 0o644); err != nil {
			return Media{}, err
		}
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(dst)
		}
	}()

	media, err := mediaFromFile(p.Dir, dst)
	if err != nil {
		return Media{}, err
	}
	dstInfo, err := os.Stat(dst)
	if err != nil {
		return Media{}, err
	}
	index := readObjectIndex(p)
	index[media.Path] = objectIndexEntry{Size: dstInfo.Size(), Mtime: dstInfo.ModTime().UnixNano(), Hash: hash}
	if err := writeObjectIndex(p, index); err != nil {
		return Media{}, err
	}
	if err := s.Touch(id); err != nil {
		return Media{}, err
	}
	history, err := s.History(id)
	if err != nil {
		return Media{}, err
	}
	if _, err := s.CommitMediaState(id, history.Head, CommitMeta{Actor: "human", Summary: "Uploaded media"}); err != nil {
		return Media{}, err
	}
	committed = true
	return media, nil
}

func (s *Store) SaveChatImage(id, originalName, mime string, data []byte) (llm.ImageRef, error) {
	if !llm.LooksLikeImage(data) {
		return llm.ImageRef{}, errors.New("file is not a readable image")
	}
	if mime == "" {
		mime = llm.DetectImageMIME(data)
	}
	name := safeName(originalName)
	if name == "" {
		name = "image"
	}
	ext := strings.ToLower(filepath.Ext(name))
	want := extForImageMIME(mime)
	if ext == "" || kindForExt(ext) != "image" {
		name = strings.TrimSuffix(name, ext) + want
	}
	media, err := s.SaveUpload(id, name, bytes.NewReader(data))
	if err != nil {
		return llm.ImageRef{}, err
	}
	return llm.ImageRef{
		Path: media.Path,
		MIME: mime,
		Name: media.Name,
	}, nil
}

func extForImageMIME(mime string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".jpg"
	}
}

func (s *Store) ListMedia(id string) ([]Media, error) {
	if s.pg != nil {
		if _, err := s.Get(id); err != nil {
			return nil, err
		}
		rows, err := s.pg.Query(context.Background(), `SELECT a.id,a.current_version_id,a.display_name,a.logical_path,a.kind,
			o.mime_type,o.byte_size,COALESCE((v.probe->>'duration')::double precision,0),
			COALESCE((v.probe->>'width')::integer,0),COALESCE((v.probe->>'height')::integer,0),a.updated_at,a.origin
			FROM assets a JOIN asset_versions v ON v.id=a.current_version_id JOIN storage_objects o ON o.id=v.storage_object_id
			WHERE a.project_id=$1 AND a.deleted_at IS NULL AND o.state='ready' ORDER BY a.updated_at DESC`, id)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []Media{}
		for rows.Next() {
			var m Media
			if err := rows.Scan(&m.ID, &m.VersionID, &m.Name, &m.Path, &m.Kind, &m.ContentType, &m.Bytes, &m.Duration, &m.Width, &m.Height, &m.ModifiedAt, &m.Origin); err != nil {
				return nil, err
			}
			out = append(out, m)
		}
		return out, rows.Err()
	}
	p, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	origins := readMediaOrigins(p)
	var out []Media
	err = filepath.WalkDir(p.Dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if path != p.Dir && (strings.HasPrefix(d.Name(), ".") || d.Name() == "exports") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 || !isMediaName(d.Name()) {
			return nil
		}
		m, err := mediaFromFile(p.Dir, path)
		if err != nil {
			return err
		}
		m.Origin = origins[m.Path]
		out = append(out, m)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModifiedAt.After(out[j].ModifiedAt) })
	if out == nil {
		out = []Media{}
	}
	return out, nil
}

func (s *Store) ResolveFile(id, rel string) (string, error) {
	if s.pg != nil {
		p, err := s.Get(id)
		if err != nil {
			return "", err
		}
		clean, err := cleanProjectPath(rel)
		if err != nil {
			return "", err
		}
		if clean == "" {
			return "", errors.New("path must name a file")
		}
		var key string
		err = s.pg.QueryRow(context.Background(), `SELECT o.object_key FROM assets a JOIN asset_versions v ON v.id=a.current_version_id JOIN storage_objects o ON o.id=v.storage_object_id WHERE a.project_id=$1 AND a.logical_path=$2 AND a.deleted_at IS NULL AND o.state='ready'`, id, clean).Scan(&key)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		if err != nil {
			return "", err
		}
		full := filepath.Join(p.Dir, filepath.FromSlash(clean))
		if _, err = os.Stat(full); err == nil {
			return full, nil
		}
		if err = s.objects.Download(context.Background(), key, full); err != nil {
			return "", err
		}
		return full, nil
	}
	p, err := s.Get(id)
	if err != nil {
		return "", err
	}
	rel = filepath.Clean(filepath.FromSlash(rel))
	if rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("invalid media path")
	}
	full := filepath.Join(p.Dir, rel)
	info, err := os.Lstat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrNotFound
		}
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("media path is not a regular file")
	}
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", err
	}
	relToRoot, err := filepath.Rel(p.Dir, real)
	if err != nil || relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(filepath.Separator)) {
		return "", errors.New("media path escapes the project")
	}
	return real, nil
}

func (s *Store) PrepareExport(id, name, ext string) (Media, error) {
	p, err := s.Get(id)
	if err != nil {
		return Media{}, err
	}
	name = safeName(name)
	if name == "" {
		name = "export"
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	dir := filepath.Join(p.Dir, "exports")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Media{}, err
	}
	abs := availablePath(dir, name+ext)
	rel, err := filepath.Rel(p.Dir, abs)
	if err != nil {
		return Media{}, err
	}
	return Media{Name: filepath.Base(abs), Path: filepath.ToSlash(rel)}, nil
}

func (s *Store) StatFile(id, rel string) (Media, error) {
	if s.pg != nil {
		items, err := s.ListMedia(id)
		if err != nil {
			return Media{}, err
		}
		clean := filepath.ToSlash(strings.TrimSpace(rel))
		for _, m := range items {
			if m.Path == clean {
				return m, nil
			}
		}
		p, err := s.Get(id)
		if err != nil {
			return Media{}, err
		}
		abs := filepath.Join(p.Dir, filepath.FromSlash(clean))
		if info, statErr := os.Stat(abs); statErr == nil && info.Mode().IsRegular() {
			return s.commitPGFile(context.Background(), id, filepath.Base(clean), abs, "export")
		}
		return Media{}, ErrNotFound
	}
	p, err := s.Get(id)
	if err != nil {
		return Media{}, err
	}
	full, err := s.ResolveFile(id, rel)
	if err != nil {
		return Media{}, err
	}
	media, err := mediaFromFile(p.Dir, full)
	if err == nil {
		media.Origin = readMediaOrigins(p)[media.Path]
	}
	return media, err
}

func (s *Store) DeleteFile(id, rel string) error {
	if s.pg != nil {
		if _, err := s.Get(id); err != nil {
			return err
		}
		clean := filepath.ToSlash(strings.TrimSpace(rel))
		result, err := s.pg.Exec(context.Background(), `UPDATE assets SET deleted_at=now(),updated_at=now() WHERE project_id=$1 AND logical_path=$2 AND deleted_at IS NULL`, id, clean)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return ErrNotFound
		}
		_ = os.Remove(filepath.Join(s.root, id, filepath.FromSlash(clean)))
		return s.Touch(id)
	}
	p, err := s.Get(id)
	if err != nil {
		return err
	}
	full, err := s.ResolveFile(id, rel)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}
	cleanRel := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	s.uploadMu.Lock()
	origins := readMediaOrigins(p)
	delete(origins, cleanRel)
	originErr := writeMediaOrigins(p, origins)
	s.uploadMu.Unlock()
	if originErr != nil {
		return originErr
	}
	return s.Touch(id)
}

// Delete removes a project and every file under its workspace: media,
// transcripts, chats, timeline, history, and exports.
func (s *Store) Delete(id string) error {
	if s.pg != nil {
		if _, err := s.Get(id); err != nil {
			return err
		}
		tx, err := s.pg.Begin(context.Background())
		if err != nil {
			return err
		}
		defer tx.Rollback(context.Background())
		query := `UPDATE projects SET state='deleting',updated_at=now() WHERE id=$1 AND state='active'`
		args := []any{id}
		if s.ownerID != uuid.Nil {
			query += ` AND owner_id=$2`
			args = append(args, s.ownerID)
		}
		result, err := tx.Exec(context.Background(), query, args...)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return ErrNotFound
		}
		jobID := uuid.New()
		_, err = tx.Exec(context.Background(), `INSERT INTO jobs(id,project_id,job_type,state,idempotency_key) VALUES($1,$2,'project_purge','queued','project-purge') ON CONFLICT(project_id,idempotency_key) DO UPDATE SET state='queued',next_run_at=now(),updated_at=now()`, jobID, id)
		if err != nil {
			return err
		}
		return tx.Commit(context.Background())
	}
	id = strings.TrimSpace(id)
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.data[id]
	if !ok {
		return ErrNotFound
	}
	dir := filepath.Clean(p.Dir)
	want := filepath.Clean(filepath.Join(s.root, id))
	if dir != want || filepath.Base(dir) != id {
		return fmt.Errorf("project directory is invalid")
	}
	delete(s.data, id)
	if err := os.RemoveAll(dir); err != nil {
		if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
			s.data[id] = p
			return err
		}
	}
	return nil
}

func (s *Store) commitPGFile(ctx context.Context, projectID, originalName, source, actor string) (Media, error) {
	p, err := s.Get(projectID)
	if err != nil {
		return Media{}, err
	}
	name := safeName(originalName)
	if name == "" {
		return Media{}, errors.New("uploaded file needs a valid filename")
	}
	if !isMediaName(name) {
		return Media{}, fmt.Errorf("unsupported media type %q", filepath.Ext(name))
	}
	ext := strings.ToLower(filepath.Ext(name))
	mimeType := contentType(ext)
	kind := kindForExt(ext)
	obj, err := s.objects.UploadFile(ctx, projectID, source, mimeType)
	if err != nil {
		return Media{}, fmt.Errorf("upload durable object: %w", err)
	}
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return Media{}, err
	}
	defer tx.Rollback(ctx)
	var objectID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM storage_objects WHERE project_id=$1 AND sha256=$2 AND state='ready'`, projectID, obj.SHA256).Scan(&objectID)
	if errors.Is(err, pgx.ErrNoRows) {
		objectID = uuid.New()
		_, err = tx.Exec(ctx, `INSERT INTO storage_objects(id,project_id,object_key,sha256,byte_size,mime_type,etag,state) VALUES($1,$2,$3,$4,$5,$6,$7,'ready')`, objectID, projectID, obj.Key, obj.SHA256, obj.Bytes, mimeType, obj.ETag)
	}
	if err != nil {
		return Media{}, err
	}
	logical, err := uniquePGPath(ctx, tx, projectID, name)
	if err != nil {
		return Media{}, err
	}
	assetID, versionID := uuid.New(), uuid.New()
	ownerID, err := uuid.Parse(p.OwnerID)
	if err != nil {
		return Media{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO assets(id,project_id,display_name,logical_path,kind,origin,created_by) VALUES($1,$2,$3,$4,$5,$6,$7)`, assetID, projectID, name, logical, kind, actor, ownerID); err != nil {
		return Media{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO asset_versions(id,asset_id,version_no,storage_object_id,created_by) VALUES($1,$2,1,$3,$4)`, versionID, assetID, objectID, ownerID); err != nil {
		return Media{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE assets SET current_version_id=$2 WHERE id=$1`, assetID, versionID); err != nil {
		return Media{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE projects SET updated_at=now() WHERE id=$1`, projectID); err != nil {
		return Media{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Media{}, err
	}
	media := Media{ID: assetID.String(), VersionID: versionID.String(), Name: name, Path: logical, Kind: kind, ContentType: mimeType, Bytes: obj.Bytes, ModifiedAt: time.Now().UTC(), Origin: actor}
	if h, hErr := s.History(projectID); hErr == nil {
		_, _ = s.CommitMediaState(projectID, h.Head, CommitMeta{Actor: actor, Summary: "Uploaded media"})
	}
	return media, nil
}

// syncPGWorkspace promotes media produced in the temporary per-project
// workspace into immutable durable asset versions. It deliberately ignores
// .parallax metadata and .scratch intermediates; neither is authoritative.
// PublishWorkspaceMedia makes a completed output available to the bin before the
// agent's timeline transaction finishes. Only this file is promoted: parallel
// generators may still be writing other workspace files.
func (s *Store) PublishWorkspaceMedia(ctx context.Context, projectID, rel string) error {
	if s.pg == nil {
		return nil
	}
	clean, err := cleanProjectPath(rel)
	if err != nil {
		return err
	}
	if clean == "" {
		return errors.New("empty media path")
	}
	project, err := s.Get(projectID)
	if err != nil {
		return err
	}
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := s.syncPGWorkspacePaths(ctx, tx, project, clean); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) syncPGWorkspace(ctx context.Context, tx pgx.Tx, project Project) error {
	return s.syncPGWorkspacePaths(ctx, tx, project, "")
}

func (s *Store) syncPGWorkspacePaths(ctx context.Context, tx pgx.Tx, project Project, onlyPath string) error {
	ownerID, err := uuid.Parse(project.OwnerID)
	if err != nil {
		return err
	}
	return filepath.WalkDir(project.Dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != project.Dir && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || !isMediaName(entry.Name()) {
			return nil
		}
		rel, err := filepath.Rel(project.Dir, path)
		if err != nil {
			return err
		}
		rel, err = cleanProjectPath(rel)
		if err != nil || rel == "" {
			return err
		}
		if onlyPath != "" && rel != onlyPath {
			return nil
		}
		hash, err := HashFile(path)
		if err != nil {
			return err
		}
		var assetID, currentVersion uuid.UUID
		var currentHash string
		err = tx.QueryRow(ctx, `
			SELECT a.id,a.current_version_id,o.sha256
			FROM assets a
			JOIN asset_versions v ON v.id=a.current_version_id
			JOIN storage_objects o ON o.id=v.storage_object_id
			WHERE a.project_id=$1 AND a.logical_path=$2 AND a.deleted_at IS NULL
			FOR UPDATE`, project.ID, rel).Scan(&assetID, &currentVersion, &currentHash)
		if err == nil && currentHash == hash {
			_ = currentVersion
			return nil
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		mimeType := contentType(strings.ToLower(filepath.Ext(entry.Name())))
		object, err := s.objects.UploadFile(ctx, project.ID, path, mimeType)
		if err != nil {
			return fmt.Errorf("upload workspace output %s: %w", rel, err)
		}
		var objectID uuid.UUID
		err = tx.QueryRow(ctx, `SELECT id FROM storage_objects WHERE project_id=$1 AND sha256=$2 AND state='ready'`, project.ID, object.SHA256).Scan(&objectID)
		if errors.Is(err, pgx.ErrNoRows) {
			objectID = uuid.New()
			_, err = tx.Exec(ctx, `INSERT INTO storage_objects(id,project_id,object_key,sha256,byte_size,mime_type,etag,state) VALUES($1,$2,$3,$4,$5,$6,$7,'ready')`, objectID, project.ID, object.Key, object.SHA256, object.Bytes, mimeType, object.ETag)
		}
		if err != nil {
			return err
		}
		versionID := uuid.New()
		if assetID == uuid.Nil {
			assetID = uuid.New()
			_, err = tx.Exec(ctx, `INSERT INTO assets(id,project_id,display_name,logical_path,kind,origin,created_by) VALUES($1,$2,$3,$4,$5,'generated',$6)`, assetID, project.ID, entry.Name(), rel, kindForExt(filepath.Ext(entry.Name())), ownerID)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `INSERT INTO asset_versions(id,asset_id,version_no,storage_object_id,created_by) VALUES($1,$2,1,$3,$4)`, versionID, assetID, objectID, ownerID)
		} else {
			_, err = tx.Exec(ctx, `INSERT INTO asset_versions(id,asset_id,version_no,storage_object_id,created_by) SELECT $1,$2,COALESCE(max(version_no),0)+1,$3,$4 FROM asset_versions WHERE asset_id=$2`, versionID, assetID, objectID, ownerID)
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE assets SET current_version_id=$2,display_name=$3,updated_at=now() WHERE id=$1`, assetID, versionID, entry.Name())
		return err
	})
}

type pgQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func uniquePGPath(ctx context.Context, q pgQuerier, projectID, name string) (string, error) {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; i < 10000; i++ {
		candidate := "media/" + name
		if i > 1 {
			candidate = fmt.Sprintf("media/%s_%d%s", base, i, ext)
		}
		var exists bool
		err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM assets WHERE project_id=$1 AND logical_path=$2 AND deleted_at IS NULL)`, projectID, candidate).Scan(&exists)
		if err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}
	}
	return "", errors.New("could not allocate media path")
}

func cleanProjectPath(rel string) (string, error) {
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	if clean == "." {
		return "", nil
	}
	if strings.HasPrefix(clean, "../") || clean == ".." || filepath.IsAbs(filepath.FromSlash(clean)) {
		return "", errors.New("path leaves project")
	}
	return clean, nil
}

func writeProject(p Project) error {
	metaDir := filepath.Join(p.Dir, ".parallax")
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(metaDir, "project.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(metaDir, "project.json"))
}

func readProject(dir string) (Project, error) {
	b, err := os.ReadFile(filepath.Join(dir, ".parallax", "project.json"))
	if err != nil {
		return Project{}, err
	}
	var p Project
	if err := json.Unmarshal(b, &p); err != nil {
		return Project{}, err
	}
	if p.ID == "" || filepath.Base(dir) != p.ID {
		return Project{}, errors.New("invalid project metadata")
	}
	p.Dir = dir
	return p, nil
}

func mediaFromFile(root, path string) (Media, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Media{}, err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return Media{}, err
	}
	rel = filepath.ToSlash(rel)
	ext := strings.ToLower(filepath.Ext(path))
	return Media{
		ID:          fmt.Sprintf("%x", shortHash(rel)),
		Name:        info.Name(),
		Path:        rel,
		Kind:        kindForExt(ext),
		ContentType: contentType(ext),
		Bytes:       info.Size(),
		ModifiedAt:  info.ModTime().UTC(),
	}, nil
}

func shortHash(s string) [8]byte {
	var out [8]byte
	for i := range []byte(s) {
		out[i%len(out)] = out[i%len(out)]*31 + s[i]
	}
	return out
}

func safeName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune("._- ", r):
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return strings.Trim(b.String(), ". ")
}

func availablePath(dir, name string) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 0; ; i++ {
		candidate := filepath.Join(dir, name)
		if i > 0 {
			candidate = filepath.Join(dir, fmt.Sprintf("%s-%d%s", base, i, ext))
		}
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

func contentType(ext string) string {
	if value := mime.TypeByExtension(ext); value != "" {
		return value
	}
	return "application/octet-stream"
}

func KindForExt(ext string) string {
	return kindForExt(ext)
}

// SanitizeMediaName returns the filesystem-safe form used for uploaded media.
func SanitizeMediaName(name string) string { return safeName(name) }

// IsSupportedMediaName reports whether name has an accepted media extension.
func IsSupportedMediaName(name string) bool { return isMediaName(name) }

func kindForExt(ext string) string {
	switch ext {
	case ".mp4", ".mov", ".mkv", ".webm", ".avi", ".m4v", ".ts", ".mts":
		return "video"
	case ".mp3", ".wav", ".aac", ".flac", ".m4a", ".ogg", ".opus":
		return "audio"
	case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".bmp", ".tif", ".tiff":
		return "image"
	case ".srt", ".ass", ".ssa", ".vtt", ".lrc":
		return "subtitle"
	default:
		return "file"
	}
}

func isMediaName(name string) bool { return kindForExt(strings.ToLower(filepath.Ext(name))) != "file" }

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
