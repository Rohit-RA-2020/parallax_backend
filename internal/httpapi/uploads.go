package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tus/tusd/v2/pkg/filelocker"
	"github.com/tus/tusd/v2/pkg/filestore"
	tusd "github.com/tus/tusd/v2/pkg/handler"

	"parallax/internal/ffmpeg"
	"parallax/internal/projects"
)

const (
	defaultUploadExpiry          = 24 * time.Hour
	defaultUploadStatusRetention = 7 * 24 * time.Hour
	defaultMaxActiveUploads      = 8
	defaultMaxProjectUploads     = 4
	uploadNetworkTimeout         = 60 * time.Second
)

type UploadManagerConfig struct {
	Workspace       string
	Projects        *projects.Store
	Bins            ffmpeg.Bins
	Logger          *slog.Logger
	MaxSize         int64
	Expiry          time.Duration
	StatusRetention time.Duration
	MaxActive       int
	MaxPerProject   int
	OnReady         func(projectID string, media projects.Media, uploadMs int64)
	Validate        func(context.Context, string, string) error
}

type uploadRecord struct {
	ID        string          `json:"id"`
	State     string          `json:"state"`
	ProjectID string          `json:"project_id"`
	Filename  string          `json:"filename"`
	Filetype  string          `json:"filetype,omitempty"`
	Offset    int64           `json:"offset"`
	Size      int64           `json:"size"`
	Error     string          `json:"error,omitempty"`
	Attempts  int             `json:"attempts,omitempty"`
	NextTryAt time.Time       `json:"next_try_at,omitempty"`
	Media     *projects.Media `json:"media,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

type UploadStats struct {
	Active     int   `json:"active_uploads"`
	Finalizing int   `json:"pending_finalizations"`
	FreeBytes  int64 `json:"workspace_free_bytes"`
	Reserved   int64 `json:"reserved_upload_bytes"`
}

type uploadStatusResponse struct {
	ID        string         `json:"id"`
	State     string         `json:"state"`
	ProjectID string         `json:"project_id"`
	Filename  string         `json:"filename"`
	Filetype  string         `json:"filetype,omitempty"`
	Offset    int64          `json:"offset"`
	Size      int64          `json:"size"`
	Error     string         `json:"error,omitempty"`
	Media     *mediaResponse `json:"media,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// UploadManager owns the resumable transport and the durable transition from a
// completed tus resource into project media.
type UploadManager struct {
	cfg       UploadManagerConfig
	root      string
	statusDir string
	store     filestore.FileStore
	handler   *tusd.Handler

	mu           sync.Mutex
	reservations map[string]int64
	queued       map[string]bool
	finalize     chan string
	stop         chan struct{}
	wg           sync.WaitGroup
}

func NewUploadManager(cfg UploadManagerConfig) (*UploadManager, error) {
	if cfg.Projects == nil {
		return nil, errors.New("upload manager requires a project store")
	}
	if strings.TrimSpace(cfg.Workspace) == "" {
		return nil, errors.New("upload manager requires a workspace")
	}
	if cfg.MaxSize <= 0 {
		cfg.MaxSize = DefaultMaxUploadBytes
	}
	if cfg.Expiry <= 0 {
		cfg.Expiry = defaultUploadExpiry
	}
	if cfg.StatusRetention <= 0 {
		cfg.StatusRetention = defaultUploadStatusRetention
	}
	if cfg.MaxActive <= 0 {
		cfg.MaxActive = defaultMaxActiveUploads
	}
	if cfg.MaxPerProject <= 0 {
		cfg.MaxPerProject = defaultMaxProjectUploads
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	root := filepath.Join(cfg.Workspace, ".parallax", "uploads")
	statusDir := filepath.Join(cfg.Workspace, ".parallax", "upload-status")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(statusDir, 0o700); err != nil {
		return nil, err
	}
	m := &UploadManager{
		cfg: cfg, root: root, statusDir: statusDir,
		store: filestore.New(root), reservations: map[string]int64{},
		queued: map[string]bool{}, finalize: make(chan string, 128), stop: make(chan struct{}),
	}
	m.store.DirModePerm = 0o700
	m.store.FileModePerm = 0o600
	composer := tusd.NewStoreComposer()
	composer.UseCore(m.store)
	composer.UseTerminater(m.store)
	filelocker.New(root).UseIn(composer)
	h, err := tusd.NewHandler(tusd.Config{
		BasePath:                   "/v1/uploads/",
		StoreComposer:              composer,
		MaxSize:                    cfg.MaxSize,
		DisableDownload:            true,
		DisableConcatenation:       true,
		NotifyCompleteUploads:      true,
		NotifyTerminatedUploads:    true,
		NetworkTimeout:             uploadNetworkTimeout,
		AcquireLockTimeout:         20 * time.Second,
		PreUploadCreateCallback:    m.preCreate,
		PreUploadTerminateCallback: m.preTerminate,
		EnableExperimentalProtocol: false,
	})
	if err != nil {
		return nil, err
	}
	m.handler = h
	m.wg.Add(4)
	go m.completionLoop()
	go m.terminationLoop()
	go m.finalizeLoop()
	go m.maintenanceLoop()
	m.reconcile()
	m.reap()
	return m, nil
}

func (m *UploadManager) preTerminate(hook tusd.HookEvent) (tusd.HTTPResponse, error) {
	if hook.Upload.Size > 0 && hook.Upload.Offset == hook.Upload.Size {
		return tusd.HTTPResponse{}, uploadHTTPError("ERR_UPLOAD_FINALIZING", "completed uploads cannot be cancelled while finalizing", http.StatusConflict)
	}
	if rec, err := m.readRecord(hook.Upload.ID); err == nil && rec.State != "receiving" {
		return tusd.HTTPResponse{}, uploadHTTPError("ERR_UPLOAD_FINALIZING", "upload can no longer be cancelled", http.StatusConflict)
	}
	return tusd.HTTPResponse{}, nil
}

func (m *UploadManager) Handler() http.Handler { return m.handler }

func (s *Server) handleUploadStatus(w http.ResponseWriter, r *http.Request) {
	if s.Uploads == nil {
		writeError(w, http.StatusNotFound, "upload not found")
		return
	}
	rec, err := s.Uploads.Status(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "upload not found")
		return
	}
	resp := uploadStatusResponse{ID: rec.ID, State: rec.State, ProjectID: rec.ProjectID, Filename: rec.Filename, Filetype: rec.Filetype, Offset: rec.Offset, Size: rec.Size, Error: rec.Error, CreatedAt: rec.CreatedAt, UpdatedAt: rec.UpdatedAt}
	if rec.Media != nil {
		items := []projects.Media{*rec.Media}
		s.attachDurations(rec.ProjectID, items)
		wrapped := s.mediaResponses(rec.ProjectID, items)
		if len(wrapped) > 0 {
			resp.Media = &wrapped[0]
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (m *UploadManager) Close() {
	if m == nil {
		return
	}
	select {
	case <-m.stop:
		return
	default:
		close(m.stop)
	}
	m.wg.Wait()
}

func (m *UploadManager) preCreate(hook tusd.HookEvent) (tusd.HTTPResponse, tusd.FileInfoChanges, error) {
	info := hook.Upload
	if info.SizeIsDeferred || info.Size <= 0 {
		return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, uploadHTTPError("ERR_UPLOAD_LENGTH", "Upload-Length must be a positive known size", http.StatusBadRequest)
	}
	projectID := strings.TrimSpace(info.MetaData["project_id"])
	filename := projects.SanitizeMediaName(info.MetaData["filename"])
	filetype := strings.TrimSpace(info.MetaData["filetype"])
	if projectID == "" || filename == "" {
		return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, uploadHTTPError("ERR_UPLOAD_METADATA", "project_id and filename metadata are required", http.StatusBadRequest)
	}
	if _, err := m.cfg.Projects.Get(projectID); err != nil {
		return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, uploadHTTPError("ERR_UPLOAD_PROJECT", "project not found", http.StatusNotFound)
	}
	if !projects.IsSupportedMediaName(filename) {
		return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, uploadHTTPError("ERR_UPLOAD_TYPE", "unsupported media type", http.StatusUnsupportedMediaType)
	}
	if info.Size > m.cfg.MaxSize {
		return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, uploadHTTPError("ERR_UPLOAD_SIZE", "upload exceeds the configured maximum", http.StatusRequestEntityTooLarge)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	active, projectActive, reserved := m.activeLocked(projectID)
	if active >= m.cfg.MaxActive || projectActive >= m.cfg.MaxPerProject {
		return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, uploadHTTPError("ERR_UPLOAD_BUSY", "too many active uploads", http.StatusTooManyRequests)
	}
	headroom := info.Size / 4
	if headroom < 2<<30 {
		headroom = 2 << 30
	}
	if err := projects.EnsureDiskSpace(m.root, reserved+info.Size+headroom); err != nil {
		return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, uploadHTTPError("ERR_UPLOAD_SPACE", err.Error(), http.StatusInsufficientStorage)
	}
	id := newUploadID()
	now := time.Now().UTC()
	m.reservations[id] = info.Size
	meta := tusd.MetaData{
		"project_id": projectID,
		"filename":   filename,
		"filetype":   filetype,
		"created_at": now.Format(time.RFC3339Nano),
	}
	_ = m.writeRecord(uploadRecord{ID: id, State: "receiving", ProjectID: projectID, Filename: filename, Filetype: filetype, Size: info.Size, CreatedAt: now, UpdatedAt: now})
	go m.confirmCreation(id)
	return tusd.HTTPResponse{}, tusd.FileInfoChanges{ID: id, MetaData: meta}, nil
}

func (m *UploadManager) confirmCreation(id string) {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-m.stop:
		return
	case <-timer.C:
	}
	if _, err := os.Stat(filepath.Join(m.root, id+".info")); err == nil {
		return
	}
	if rec, err := m.readRecord(id); err == nil && rec.State != "receiving" {
		return
	}
	m.mu.Lock()
	delete(m.reservations, id)
	m.mu.Unlock()
	_ = os.Remove(m.recordPath(id))
}

func uploadHTTPError(code, message string, status int) error {
	return tusd.NewError(code, message, status)
}

func newUploadID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func (m *UploadManager) completionLoop() {
	defer m.wg.Done()
	for {
		select {
		case <-m.stop:
			return
		case event := <-m.handler.CompleteUploads:
			m.markFinalizing(event.Upload)
		}
	}
}

func (m *UploadManager) terminationLoop() {
	defer m.wg.Done()
	for {
		select {
		case <-m.stop:
			return
		case event := <-m.handler.TerminatedUploads:
			m.mu.Lock()
			delete(m.reservations, event.Upload.ID)
			m.mu.Unlock()
			rec, _ := m.readRecord(event.Upload.ID)
			if rec.ID != "" && rec.State != "ready" {
				rec.State, rec.Error, rec.UpdatedAt = "expired", "upload cancelled", time.Now().UTC()
				_ = m.writeRecord(rec)
			}
		}
	}
}

func (m *UploadManager) markFinalizing(info tusd.FileInfo) {
	rec, _ := m.readRecord(info.ID)
	if rec.ID == "" {
		rec = recordFromInfo(info)
	}
	rec.State, rec.Offset, rec.Size, rec.UpdatedAt = "finalizing", info.Offset, info.Size, time.Now().UTC()
	_ = m.writeRecord(rec)
	m.enqueue(info.ID)
}

func (m *UploadManager) enqueue(id string) {
	m.mu.Lock()
	if m.queued[id] {
		m.mu.Unlock()
		return
	}
	m.queued[id] = true
	m.mu.Unlock()
	select {
	case m.finalize <- id:
	case <-m.stop:
	default:
		m.mu.Lock()
		delete(m.queued, id)
		m.mu.Unlock()
	}
}

func (m *UploadManager) finalizeLoop() {
	defer m.wg.Done()
	for {
		select {
		case <-m.stop:
			return
		case id := <-m.finalize:
			m.finalizeOne(id)
			m.mu.Lock()
			delete(m.queued, id)
			m.mu.Unlock()
		}
	}
}

func (m *UploadManager) finalizeOne(id string) {
	rec, err := m.readRecord(id)
	if err != nil || rec.State == "ready" || rec.State == "failed" || rec.State == "expired" {
		return
	}
	upload, err := m.store.GetUpload(context.Background(), id)
	if err != nil {
		m.fail(rec, "completed upload is missing: "+err.Error())
		return
	}
	info, err := upload.GetInfo(context.Background())
	if err != nil || info.Offset != info.Size {
		return
	}
	source := info.Storage[filestore.StorageKeyPath]
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	validate := m.cfg.Validate
	if validate == nil {
		validate = func(ctx context.Context, source, name string) error {
			return validateUploadedMedia(ctx, m.cfg.Bins, source, name)
		}
	}
	if err := validate(ctx, source, rec.Filename); err != nil {
		if latest, readErr := m.readRecord(id); readErr == nil && latest.State == "expired" {
			return
		}
		m.fail(rec, "media validation failed: "+err.Error())
		m.removeTusFiles(info)
		return
	}
	if latest, err := m.readRecord(id); err == nil && latest.State == "expired" {
		m.removeTusFiles(info)
		return
	}
	media, err := m.cfg.Projects.FinalizeUpload(rec.ProjectID, rec.Filename, source)
	if err != nil {
		m.retry(rec, info, err)
		return
	}
	now := time.Now().UTC()
	rec.State, rec.Offset, rec.Size, rec.Error, rec.Media, rec.UpdatedAt = "ready", info.Size, info.Size, "", &media, now
	_ = m.writeRecord(rec)
	m.removeTusFiles(info)
	m.mu.Lock()
	delete(m.reservations, id)
	m.mu.Unlock()
	if m.cfg.OnReady != nil {
		uploadMs := now.Sub(rec.CreatedAt).Milliseconds()
		if uploadMs < 1 {
			uploadMs = 1
		}
		m.cfg.OnReady(rec.ProjectID, media, uploadMs)
	}
	m.cfg.Logger.Info("upload ready", "upload", id, "project", rec.ProjectID, "path", media.Path, "bytes", media.Bytes)
}

func (m *UploadManager) retry(rec uploadRecord, info tusd.FileInfo, err error) {
	rec.Attempts++
	if rec.Attempts >= 5 {
		m.fail(rec, "finalization failed after 5 attempts: "+err.Error())
		m.removeTusFiles(info)
		return
	}
	delay := time.Duration(1<<min(rec.Attempts-1, 6)) * 5 * time.Second
	rec.State, rec.Error = "finalizing", err.Error()
	rec.NextTryAt, rec.UpdatedAt = time.Now().UTC().Add(delay), time.Now().UTC()
	_ = m.writeRecord(rec)
	m.cfg.Logger.Warn("upload finalization will retry", "upload", rec.ID, "attempt", rec.Attempts, "after", delay, "err", err)
}

func validateUploadedMedia(ctx context.Context, bins ffmpeg.Bins, source, name string) error {
	kind := projects.KindForExt(strings.ToLower(filepath.Ext(name)))
	if kind == "subtitle" {
		f, err := os.Open(source)
		if err != nil {
			return err
		}
		defer f.Close()
		body, err := io.ReadAll(io.LimitReader(f, 2<<20))
		if err != nil {
			return err
		}
		if len(body) == 0 || !utf8.Valid(body) || strings.IndexByte(string(body), 0) >= 0 {
			return errors.New("subtitle is empty or not UTF-8 text")
		}
		return nil
	}
	_, err := ffmpeg.ProbeMedia(ctx, bins, filepath.Dir(source), filepath.Base(source))
	return err
}

func (m *UploadManager) fail(rec uploadRecord, message string) {
	rec.State, rec.Error, rec.UpdatedAt = "failed", strings.TrimSpace(message), time.Now().UTC()
	_ = m.writeRecord(rec)
	m.mu.Lock()
	delete(m.reservations, rec.ID)
	m.mu.Unlock()
	m.cfg.Logger.Error("upload finalization", "upload", rec.ID, "project", rec.ProjectID, "err", rec.Error)
}

func (m *UploadManager) removeTusFiles(info tusd.FileInfo) {
	_ = os.Remove(info.Storage[filestore.StorageKeyPath])
	_ = os.Remove(info.Storage[filestore.StorageKeyInfoPath])
	_ = os.Remove(filepath.Join(m.root, info.ID+".lock"))
}

func recordFromInfo(info tusd.FileInfo) uploadRecord {
	created, _ := time.Parse(time.RFC3339Nano, info.MetaData["created_at"])
	if created.IsZero() {
		created = time.Now().UTC()
	}
	state := "receiving"
	if info.Size > 0 && info.Offset == info.Size {
		state = "finalizing"
	}
	return uploadRecord{ID: info.ID, State: state, ProjectID: info.MetaData["project_id"], Filename: info.MetaData["filename"], Filetype: info.MetaData["filetype"], Offset: info.Offset, Size: info.Size, CreatedAt: created, UpdatedAt: time.Now().UTC()}
}

func (m *UploadManager) Status(id string) (uploadRecord, error) {
	id = strings.TrimSpace(id)
	if id == "" || strings.ContainsAny(id, `/\\`) {
		return uploadRecord{}, os.ErrNotExist
	}
	rec, err := m.readRecord(id)
	if err != nil {
		return uploadRecord{}, err
	}
	if rec.State == "receiving" {
		if upload, getErr := m.store.GetUpload(context.Background(), id); getErr == nil {
			if info, infoErr := upload.GetInfo(context.Background()); infoErr == nil {
				rec.Offset, rec.Size = info.Offset, info.Size
			}
		}
	}
	return rec, nil
}

func (m *UploadManager) recordPath(id string) string { return filepath.Join(m.statusDir, id+".json") }

func (m *UploadManager) readRecord(id string) (uploadRecord, error) {
	var rec uploadRecord
	body, err := os.ReadFile(m.recordPath(id))
	if err != nil {
		return rec, err
	}
	err = json.Unmarshal(body, &rec)
	return rec, err
}

func (m *UploadManager) writeRecord(rec uploadRecord) error {
	if rec.UpdatedAt.IsZero() {
		rec.UpdatedAt = time.Now().UTC()
	}
	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.statusDir, ".upload-status-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if _, err := tmp.Write(body); err != nil {
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, m.recordPath(rec.ID)); err != nil {
		return err
	}
	ok = true
	return nil
}

func (m *UploadManager) activeLocked(projectID string) (active, projectActive int, reserved int64) {
	for id, size := range m.reservations {
		active++
		reserved += size
		if rec, err := m.readRecord(id); err == nil && rec.ProjectID == projectID {
			projectActive++
		}
	}
	entries, _ := filepath.Glob(filepath.Join(m.root, "*.info"))
	for _, path := range entries {
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var info tusd.FileInfo
		if json.Unmarshal(body, &info) != nil {
			continue
		}
		if _, already := m.reservations[info.ID]; already {
			continue
		}
		active++
		reserved += info.Size
		if info.MetaData["project_id"] == projectID {
			projectActive++
		}
	}
	return
}

func (m *UploadManager) reconcile() {
	entries, _ := filepath.Glob(filepath.Join(m.root, "*.info"))
	for _, path := range entries {
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var info tusd.FileInfo
		if json.Unmarshal(body, &info) != nil || info.ID == "" {
			continue
		}
		if upload, getErr := m.store.GetUpload(context.Background(), info.ID); getErr == nil {
			if current, infoErr := upload.GetInfo(context.Background()); infoErr == nil {
				info = current
			}
		}
		rec, recErr := m.readRecord(info.ID)
		if recErr != nil {
			rec = recordFromInfo(info)
			_ = m.writeRecord(rec)
		}
		if info.Size > 0 && info.Offset == info.Size && rec.State != "ready" && rec.State != "failed" && !rec.NextTryAt.After(time.Now().UTC()) {
			m.markFinalizing(info)
		}
	}
}

func (m *UploadManager) maintenanceLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.reconcile()
			m.reap()
		}
	}
}

func (m *UploadManager) reap() {
	now := time.Now().UTC()
	records, _ := filepath.Glob(filepath.Join(m.statusDir, "*.json"))
	for _, path := range records {
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var rec uploadRecord
		if json.Unmarshal(body, &rec) != nil {
			continue
		}
		if (rec.State == "ready" || rec.State == "failed" || rec.State == "expired") && now.Sub(rec.UpdatedAt) > m.cfg.StatusRetention {
			_ = os.Remove(path)
			continue
		}
		if rec.State == "receiving" && now.Sub(rec.CreatedAt) > m.cfg.Expiry {
			m.terminate(rec.ID)
			rec.State, rec.Error, rec.UpdatedAt = "expired", "upload expired", now
			_ = m.writeRecord(rec)
		}
	}
	removeLegacyUploadTemps(m.cfg.Projects, now.Add(-m.cfg.Expiry))
}

func removeLegacyUploadTemps(store *projects.Store, olderThan time.Time) {
	if store == nil {
		return
	}
	for _, project := range store.List() {
		matches, _ := filepath.Glob(filepath.Join(project.Dir, "media", ".upload-*"))
		for _, path := range matches {
			if info, err := os.Stat(path); err == nil && info.ModTime().Before(olderThan) {
				_ = os.Remove(path)
			}
		}
	}
}

func (m *UploadManager) terminate(id string) {
	upload, err := m.store.GetUpload(context.Background(), id)
	if err != nil {
		return
	}
	if term := m.store.AsTerminatableUpload(upload); term != nil {
		_ = term.Terminate(context.Background())
	}
	m.mu.Lock()
	delete(m.reservations, id)
	m.mu.Unlock()
}

func (m *UploadManager) CancelProject(projectID string) error {
	entries, _ := filepath.Glob(filepath.Join(m.root, "*.info"))
	var ids []string
	for _, path := range entries {
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var info tusd.FileInfo
		if json.Unmarshal(body, &info) == nil && info.MetaData["project_id"] == projectID {
			ids = append(ids, info.ID)
			if rec, err := m.readRecord(info.ID); err == nil {
				rec.State, rec.Error, rec.UpdatedAt = "expired", "project deleted", time.Now().UTC()
				_ = m.writeRecord(rec)
			}
			m.terminate(info.ID)
		}
	}
	deadline := time.Now().Add(2 * time.Minute)
	for _, id := range ids {
		for {
			m.mu.Lock()
			busy := m.queued[id]
			m.mu.Unlock()
			if !busy {
				break
			}
			if time.Now().After(deadline) {
				return errors.New("timed out waiting for upload finalization to stop")
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	return nil
}

func (m *UploadManager) Stats() UploadStats {
	m.mu.Lock()
	active, _, reserved := m.activeLocked("")
	queued := len(m.queued)
	m.mu.Unlock()
	free, _ := projects.FreeBytes(m.root)
	return UploadStats{Active: active, Finalizing: queued, FreeBytes: free, Reserved: reserved}
}
