package transcript

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const videoSceneSchema = 1

// VideoScene is one searchable shot or sample window inside a video.
type VideoScene struct {
	ID       string  `json:"id"`
	Start    float64 `json:"start"`
	End      float64 `json:"end"`
	At       float64 `json:"at"`
	TextEN   string  `json:"text_en"`
	SpokenEN string  `json:"spoken_en,omitempty"`
}

// VideoScenes is the on-disk scene index for one file, keyed by content hash.
type VideoScenes struct {
	Schema      int          `json:"schema"`
	ContentHash string       `json:"content_hash"`
	Path        string       `json:"path"`
	Name        string       `json:"name,omitempty"`
	Duration    float64      `json:"duration,omitempty"`
	Embedded    bool         `json:"embedded,omitempty"`
	Scenes      []VideoScene `json:"scenes"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

func videoSceneFilePath(projectDir, hash string) string {
	return filepath.Join(projectDir, ".parallax", "video-scenes", hash+".json")
}

// LoadVideoScenes reads scene captions for a content hash. Missing files return (nil, nil).
func LoadVideoScenes(projectDir, hash string) (*VideoScenes, error) {
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return nil, fmt.Errorf("content hash is required")
	}
	b, err := os.ReadFile(videoSceneFilePath(projectDir, hash))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc VideoScenes
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// SaveVideoScenes writes scene captions atomically.
func SaveVideoScenes(projectDir string, doc *VideoScenes) error {
	if doc == nil || strings.TrimSpace(doc.ContentHash) == "" {
		return fmt.Errorf("video scene content hash is required")
	}
	doc.Schema = videoSceneSchema
	doc.UpdatedAt = time.Now().UTC()
	doc.Path = filepath.ToSlash(strings.TrimSpace(doc.Path))
	doc.Name = strings.TrimSpace(doc.Name)
	if doc.Scenes == nil {
		doc.Scenes = []VideoScene{}
	}
	dir := filepath.Join(projectDir, ".parallax", "video-scenes")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".scenes-*")
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
	if err := os.Rename(name, videoSceneFilePath(projectDir, doc.ContentHash)); err != nil {
		return err
	}
	ok = true
	return nil
}
