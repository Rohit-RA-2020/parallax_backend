package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Client is the durable, content-addressed media store on the backend VM.
// Its root must be mounted from a persistent volume, separate from temporary
// upload and FFmpeg workspace storage.
type Client struct {
	Root string
}

type Object struct {
	Key, SHA256, ETag, ContentType string
	Bytes                          int64
}

func New(root string) (*Client, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("durable media directory is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	return &Client{Root: abs}, nil
}

func (c *Client) Ready(_ context.Context) error {
	if c == nil || c.Root == "" {
		return errors.New("durable media storage is not configured")
	}
	info, err := os.Stat(c.Root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("durable media path is not a directory")
	}
	probe, err := os.CreateTemp(c.Root, ".ready-*")
	if err != nil {
		return err
	}
	name := probe.Name()
	_ = probe.Close()
	return os.Remove(name)
}

func HashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func ObjectKey(projectID, hash string) string {
	prefix := hash
	if len(prefix) > 2 {
		prefix = prefix[:2]
	}
	return filepath.ToSlash(filepath.Join("projects", projectID, "objects", prefix, hash))
}

func (c *Client) UploadFile(ctx context.Context, projectID, source, contentType string) (Object, error) {
	hash, size, err := HashFile(source)
	if err != nil {
		return Object{}, err
	}
	key := ObjectKey(projectID, hash)
	destination, err := c.Path(key)
	if err != nil {
		return Object{}, err
	}
	if info, statErr := os.Stat(destination); statErr == nil && info.Mode().IsRegular() && info.Size() == size {
		return Object{Key: key, SHA256: hash, Bytes: size, ETag: hash, ContentType: contentType}, nil
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return Object{}, err
	}
	in, err := os.Open(source)
	if err != nil {
		return Object{}, err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".object-*")
	if err != nil {
		return Object{}, err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := io.Copy(tmp, &contextReader{ctx: ctx, reader: in}); err != nil {
		return Object{}, err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return Object{}, err
	}
	if err := tmp.Sync(); err != nil {
		return Object{}, err
	}
	if err := tmp.Close(); err != nil {
		return Object{}, err
	}
	if err := os.Rename(tmpName, destination); err != nil {
		return Object{}, err
	}
	ok = true
	return Object{Key: key, SHA256: hash, Bytes: size, ETag: hash, ContentType: contentType}, nil
}

func (c *Client) Path(key string) (string, error) {
	if c == nil || c.Root == "" {
		return "", errors.New("durable media storage is not configured")
	}
	clean := filepath.Clean(filepath.FromSlash(strings.TrimSpace(key)))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("invalid object key")
	}
	path := filepath.Join(c.Root, clean)
	rel, err := filepath.Rel(c.Root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("object key leaves durable storage")
	}
	return path, nil
}

func (c *Client) Download(ctx context.Context, key, destination string) error {
	source, err := c.Path(key)
	if err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".download-*")
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
	if _, err := io.Copy(tmp, &contextReader{ctx: ctx, reader: in}); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, destination); err != nil {
		return err
	}
	ok = true
	return nil
}

func (c *Client) DeleteProject(_ context.Context, projectID string) error {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" || strings.ContainsAny(projectID, `/\\`) || projectID == "." || projectID == ".." {
		return fmt.Errorf("invalid project id")
	}
	path, err := c.Path(filepath.ToSlash(filepath.Join("projects", projectID)))
	if err != nil {
		return err
	}
	return os.RemoveAll(path)
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(p)
	}
}
