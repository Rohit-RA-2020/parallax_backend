package transcript

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const schema = 1

// Word is original-language speech with audio times in seconds.
type Word struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// Segment is a Whisper phrase. Text is original language; TextEN is the
// English translation used for embeddings and Director search.
type Segment struct {
	ID     string  `json:"id"`
	Start  float64 `json:"start"`
	End    float64 `json:"end"`
	Text   string  `json:"text"`
	TextEN string  `json:"text_en,omitempty"`
}

// Document is the on-disk raw transcript, keyed by content hash.
type Document struct {
	Schema      int       `json:"schema"`
	ContentHash string    `json:"content_hash"`
	Path        string    `json:"path"`
	Language    string    `json:"language,omitempty"`
	Duration    float64   `json:"duration,omitempty"`
	ASRModel    string    `json:"asr_model,omitempty"`
	AudioHash   string    `json:"audio_hash,omitempty"`
	Embedded    bool      `json:"embedded,omitempty"`
	Words       []Word    `json:"words"`
	Segments    []Segment `json:"segments"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func filePath(projectDir, hash string) string {
	return filepath.Join(projectDir, ".parallax", "transcripts", hash+".json")
}

// Load reads a transcript for a content hash. Missing files return (nil, nil).
func Load(projectDir, hash string) (*Document, error) {
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return nil, fmt.Errorf("content hash is required")
	}
	b, err := os.ReadFile(filePath(projectDir, hash))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc Document
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// Save writes the transcript atomically.
func Save(projectDir string, doc *Document) error {
	if doc == nil || strings.TrimSpace(doc.ContentHash) == "" {
		return fmt.Errorf("transcript content hash is required")
	}
	doc.Schema = schema
	doc.UpdatedAt = time.Now().UTC()
	if doc.Words == nil {
		doc.Words = []Word{}
	}
	if doc.Segments == nil {
		doc.Segments = []Segment{}
	}
	dir := filepath.Join(projectDir, ".parallax", "transcripts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".transcript-*")
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
	if err := os.Rename(name, filePath(projectDir, doc.ContentHash)); err != nil {
		return err
	}
	ok = true
	return nil
}

// FindByAudioHash returns a transcript whose soundtrack hash matches.
func FindByAudioHash(projectDir, audioHash string) (*Document, error) {
	audioHash = strings.TrimSpace(audioHash)
	if audioHash == "" {
		return nil, nil
	}
	dir := filepath.Join(projectDir, ".parallax", "transcripts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var doc Document
		if json.Unmarshal(b, &doc) != nil {
			continue
		}
		if doc.AudioHash == audioHash && len(doc.Segments) > 0 {
			return &doc, nil
		}
	}
	return nil, nil
}

// NeighborWindow is English text for embedding: previous + this + next segment.
func NeighborWindow(segments []Segment, i int) string {
	if i < 0 || i >= len(segments) {
		return ""
	}
	var parts []string
	appendEN := func(seg Segment) {
		if t := strings.TrimSpace(seg.TextEN); t != "" {
			parts = append(parts, t)
		}
	}
	if i > 0 {
		appendEN(segments[i-1])
	}
	appendEN(segments[i])
	if i+1 < len(segments) {
		appendEN(segments[i+1])
	}
	return strings.Join(parts, " ")
}
