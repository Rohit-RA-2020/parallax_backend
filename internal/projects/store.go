package projects

import (
	"crypto/rand"
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

	"parallax/internal/llm"
)

var ErrNotFound = errors.New("project not found")

type Project struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Dir       string    `json:"-"`
}

type Media struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Path        string    `json:"path"`
	Kind        string    `json:"kind"`
	ContentType string    `json:"content_type"`
	Bytes       int64     `json:"bytes"`
	Duration    float64   `json:"duration,omitempty"`
	Width       int       `json:"width,omitempty"`
	Height      int       `json:"height,omitempty"`
	ModifiedAt  time.Time `json:"modified_at"`
}

type Store struct {
	mu   sync.RWMutex
	root string
	data map[string]Project
}

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
	s.mu.RLock()
	p, ok := s.data[id]
	s.mu.RUnlock()
	if !ok {
		return Project{}, ErrNotFound
	}
	return p, nil
}

func (s *Store) Touch(id string) error {
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
	if _, err := io.Copy(tmp, src); err != nil {
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

func (s *Store) SaveChatImage(id, originalName, mime string, data []byte) (llm.ImageRef, error) {
	p, err := s.Get(id)
	if err != nil {
		return llm.ImageRef{}, err
	}
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
	dir := filepath.Join(p.Dir, ".parallax", "chat-media")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return llm.ImageRef{}, err
	}
	dst := availablePath(dir, name)
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return llm.ImageRef{}, err
	}
	rel, err := filepath.Rel(p.Dir, dst)
	if err != nil {
		return llm.ImageRef{}, err
	}
	return llm.ImageRef{
		Path: filepath.ToSlash(rel),
		MIME: mime,
		Name: filepath.Base(dst),
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
	p, err := s.Get(id)
	if err != nil {
		return nil, err
	}
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
	p, err := s.Get(id)
	if err != nil {
		return Media{}, err
	}
	full, err := s.ResolveFile(id, rel)
	if err != nil {
		return Media{}, err
	}
	return mediaFromFile(p.Dir, full)
}

func (s *Store) DeleteFile(id, rel string) error {
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
	return s.Touch(id)
}

// Delete removes a project and every file under its workspace: media,
// transcripts, chats, timeline, history, and exports.
func (s *Store) Delete(id string) error {
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
