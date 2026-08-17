package transcript

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const imageSchema = 1

// ImageCaption is the on-disk English description of one still, keyed by content hash.
type ImageCaption struct {
	Schema      int       `json:"schema"`
	ContentHash string    `json:"content_hash"`
	Path        string    `json:"path"`
	Name        string    `json:"name,omitempty"`
	TextEN      string    `json:"text_en"`
	Prompt      string    `json:"prompt,omitempty"`
	Width       int       `json:"width,omitempty"`
	Height      int       `json:"height,omitempty"`
	Model       string    `json:"model,omitempty"`
	Embedded    bool      `json:"embedded,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func imageFilePath(projectDir, hash string) string {
	return filepath.Join(projectDir, ".parallax", "image-captions", hash+".json")
}

// LoadImage reads a still caption for a content hash. Missing files return (nil, nil).
func LoadImage(projectDir, hash string) (*ImageCaption, error) {
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return nil, fmt.Errorf("content hash is required")
	}
	b, err := os.ReadFile(imageFilePath(projectDir, hash))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc ImageCaption
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// SaveImage writes a still caption atomically.
func SaveImage(projectDir string, doc *ImageCaption) error {
	if doc == nil || strings.TrimSpace(doc.ContentHash) == "" {
		return fmt.Errorf("image caption content hash is required")
	}
	doc.Schema = imageSchema
	doc.UpdatedAt = time.Now().UTC()
	doc.Path = filepath.ToSlash(strings.TrimSpace(doc.Path))
	doc.Name = strings.TrimSpace(doc.Name)
	doc.TextEN = strings.TrimSpace(doc.TextEN)
	doc.Prompt = strings.TrimSpace(doc.Prompt)
	dir := filepath.Join(projectDir, ".parallax", "image-captions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".caption-*")
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
	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, imageFilePath(projectDir, doc.ContentHash)); err != nil {
		return err
	}
	ok = true
	return nil
}
