package transcript

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"parallax/internal/llm"
	"parallax/internal/projects"
	"parallax/internal/qdrant"
)

const (
	KindTranscript    = "transcript"
	KindImage         = "image"
	maxCaptionBytes   = 8 << 20
	maxCaptionRunes   = 4000
	imageCaptionTO    = 2 * time.Minute
	imagePointSegment = "image"
)

const captionSystem = `You describe stills so they can be found later by meaning. Write one English paragraph covering subject, setting, notable objects, colors, on-image text, people, clothing, style, and mood. Quote readable text exactly. Do not say that this is an image. No preamble, labels, or markdown.`

// HasImage is true for stills that can be captioned and embedded.
func HasImage(rel string) bool {
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".bmp", ".tif", ".tiff":
		return true
	default:
		return false
	}
}

func (x *Indexer) canCaption() bool {
	return x != nil && x.Completer != nil
}

// SetImageHint stores a generation or edit prompt used when captioning that path.
func (x *Indexer) SetImageHint(projectID, rel, prompt string) {
	if x == nil {
		return
	}
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	prompt = strings.TrimSpace(prompt)
	if rel == "" || prompt == "" {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.hints == nil {
		x.hints = map[string]string{}
	}
	x.hints[statusKey(projectID, rel)] = prompt
}

func (x *Indexer) takeHint(projectID, rel string) string {
	if x == nil {
		return ""
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.hints == nil {
		return ""
	}
	key := statusKey(projectID, rel)
	prompt := x.hints[key]
	delete(x.hints, key)
	return prompt
}

func (x *Indexer) indexImage(ctx context.Context, projectID, rel string) error {
	if !x.canCaption() {
		x.Mark(projectID, rel, StateSkipped, "")
		return nil
	}
	project, err := x.Projects.Get(projectID)
	if err != nil {
		return err
	}
	abs, err := x.Projects.ResolveFile(projectID, rel)
	if err != nil {
		return err
	}
	hash, err := projects.HashFile(abs)
	if err != nil {
		return err
	}
	hint := x.takeHint(projectID, rel)
	doc, err := LoadImage(project.Dir, hash)
	if err != nil {
		return err
	}
	name := filepath.Base(rel)
	if imageComplete(doc) {
		prevPath := doc.Path
		if hint != "" && doc.Prompt != hint {
			doc.Prompt = hint
		}
		doc.Path = rel
		doc.Name = name
		if err := SaveImage(project.Dir, doc); err != nil {
			return err
		}
		if !x.canEmbed() {
			x.Mark(projectID, rel, StateReady, "")
			return nil
		}
		x.Mark(projectID, rel, StateIndexing, "")
		if err := x.upsertImage(ctx, projectID, doc, prevPath); err != nil {
			doc.Embedded = false
			_ = SaveImage(project.Dir, doc)
			x.Mark(projectID, rel, StateIndexFailed, err.Error())
			x.log().Error("image embed", "project", projectID, "path", rel, "err", err)
			return nil
		}
		doc.Embedded = true
		if err := SaveImage(project.Dir, doc); err != nil {
			return err
		}
		x.Mark(projectID, rel, StateReady, "")
		return nil
	}

	if doc == nil || strings.TrimSpace(doc.TextEN) == "" {
		x.Mark(projectID, rel, StateDescribing, "")
		data, err := os.ReadFile(abs)
		if err != nil {
			return err
		}
		if int64(len(data)) > maxCaptionBytes {
			return fmt.Errorf("image is too large to describe (max %d bytes)", maxCaptionBytes)
		}
		if !llm.LooksLikeImage(data) {
			return fmt.Errorf("file is not a readable image: %s", rel)
		}
		completer := x.Completer()
		if completer == nil {
			return fmt.Errorf("image captioner is not configured")
		}
		captionCtx, cancel := context.WithTimeout(ctx, imageCaptionTO)
		defer cancel()
		text, err := CaptionImage(captionCtx, completer, llm.ImageRef{
			Path: rel,
			MIME: llm.DetectImageMIME(data),
			Name: name,
			Data: base64.StdEncoding.EncodeToString(data),
		}, name, rel, hint)
		if err != nil {
			return err
		}
		width, height := stillDimensions(data)
		doc = &ImageCaption{
			ContentHash: hash,
			Path:        rel,
			Name:        name,
			TextEN:      text,
			Prompt:      hint,
			Width:       width,
			Height:      height,
		}
		if err := SaveImage(project.Dir, doc); err != nil {
			return err
		}
	} else {
		doc.Path = rel
		doc.Name = name
		if hint != "" {
			doc.Prompt = hint
		}
	}

	if !x.canEmbed() {
		x.Mark(projectID, rel, StateReady, "")
		return nil
	}
	x.Mark(projectID, rel, StateIndexing, "")
	if err := x.upsertImage(ctx, projectID, doc, ""); err != nil {
		doc.Embedded = false
		_ = SaveImage(project.Dir, doc)
		x.Mark(projectID, rel, StateIndexFailed, err.Error())
		x.log().Error("image embed", "project", projectID, "path", rel, "err", err)
		return nil
	}
	doc.Embedded = true
	if err := SaveImage(project.Dir, doc); err != nil {
		return err
	}
	x.Mark(projectID, rel, StateReady, "")
	return nil
}

func imageComplete(doc *ImageCaption) bool {
	return doc != nil && strings.TrimSpace(doc.TextEN) != ""
}

func (x *Indexer) upsertImage(ctx context.Context, projectID string, doc *ImageCaption, prevPath string) error {
	if x.Embeddings == nil || x.Qdrant == nil {
		x.log().Info("skip image embed", "reason", "embeddings or qdrant not configured", "path", doc.Path)
		return nil
	}
	text := imageEmbedText(doc)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	vectors, err := x.Embeddings.Embed(ctx, []string{text})
	if err != nil {
		return err
	}
	if len(vectors) == 0 || len(vectors[0]) == 0 {
		return fmt.Errorf("embed: no vectors returned")
	}
	collection := qdrant.CollectionName(projectID)
	if err := x.Qdrant.EnsureCollection(ctx, collection, len(vectors[0])); err != nil {
		return err
	}
	if prev := filepath.ToSlash(strings.TrimSpace(prevPath)); prev != "" && prev != doc.Path {
		if err := x.Qdrant.DeleteByPathAndKind(ctx, collection, prev, KindImage, false); err != nil {
			return err
		}
	}
	if err := x.Qdrant.DeleteByPathAndKind(ctx, collection, doc.Path, KindImage, false); err != nil {
		return err
	}
	payload := map[string]any{
		"kind":         KindImage,
		"path":         doc.Path,
		"name":         doc.Name,
		"content_hash": doc.ContentHash,
		"text_en":      doc.TextEN,
	}
	if doc.Width > 0 {
		payload["width"] = doc.Width
	}
	if doc.Height > 0 {
		payload["height"] = doc.Height
	}
	if strings.TrimSpace(doc.Prompt) != "" {
		payload["prompt"] = doc.Prompt
	}
	return x.Qdrant.Upsert(ctx, collection, []qdrant.Point{{
		ID:      qdrant.PointID(doc.ContentHash, imagePointSegment+":"+doc.Path),
		Vector:  vectors[0],
		Payload: payload,
	}})
}

func imageEmbedText(doc *ImageCaption) string {
	if doc == nil {
		return ""
	}
	var b strings.Builder
	if name := strings.TrimSpace(doc.Name); name != "" {
		fmt.Fprintf(&b, "Name: %s\n", name)
	}
	if path := strings.TrimSpace(doc.Path); path != "" {
		fmt.Fprintf(&b, "Path: %s\n", path)
	}
	if prompt := strings.TrimSpace(doc.Prompt); prompt != "" {
		fmt.Fprintf(&b, "Generation prompt: %s\n", prompt)
	}
	if text := strings.TrimSpace(doc.TextEN); text != "" {
		fmt.Fprintf(&b, "Description: %s", text)
	}
	return strings.TrimSpace(b.String())
}

// CaptionImage asks a vision-capable completer for an English still description.
func CaptionImage(ctx context.Context, completer llm.Completer, image llm.ImageRef, name, path, prompt string) (string, error) {
	if completer == nil {
		return "", fmt.Errorf("image captioner is not configured")
	}
	if strings.TrimSpace(image.Data) == "" {
		return "", fmt.Errorf("image bytes are required")
	}
	var user strings.Builder
	user.WriteString("Describe this still so someone could find it later by what it looks like.")
	if name = strings.TrimSpace(name); name != "" {
		fmt.Fprintf(&user, "\nFilename: %s", name)
	}
	if path = strings.TrimSpace(path); path != "" {
		fmt.Fprintf(&user, "\nPath: %s", path)
	}
	if prompt = strings.TrimSpace(prompt); prompt != "" {
		fmt.Fprintf(&user, "\nAssociated generation or edit prompt (may be incomplete after edits): %s", prompt)
	}
	temp := llm.Ptr(0.2)
	raw, err := completer.Complete(ctx, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: captionSystem},
			{Role: llm.RoleUser, Content: user.String(), Images: []llm.ImageRef{image}},
		},
		Temperature: temp,
	})
	if err != nil {
		return "", err
	}
	text := cleanCaption(raw)
	if text == "" {
		return "", fmt.Errorf("image captioner returned an empty description")
	}
	return text, nil
}

func cleanCaption(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		if i := strings.Index(s, "\n"); i >= 0 && i < 12 {
			s = s[i+1:]
		}
		s = strings.TrimSpace(strings.TrimSuffix(s, "```"))
	}
	s = strings.TrimSpace(s)
	if n := utf8.RuneCountInString(s); n > maxCaptionRunes {
		runes := []rune(s)
		s = strings.TrimSpace(string(runes[:maxCaptionRunes]))
	}
	return s
}

// GetImage loads the caption for the current bytes of a project still.
func (x *Indexer) GetImage(projectID, rel string) (*ImageCaption, error) {
	if x == nil || x.Projects == nil {
		return nil, fmt.Errorf("image captions are not configured")
	}
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if rel == "" {
		return nil, fmt.Errorf("path is required")
	}
	project, err := x.Projects.Get(projectID)
	if err != nil {
		return nil, err
	}
	abs, err := x.Projects.ResolveFile(projectID, rel)
	if err != nil {
		return nil, err
	}
	hash, err := projects.HashFile(abs)
	if err != nil {
		return nil, err
	}
	doc, err := LoadImage(project.Dir, hash)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("no caption for %s", rel)
	}
	return doc, nil
}

// SearchImages embeds an English query and returns matching stills.
func (x *Indexer) SearchImages(ctx context.Context, projectID, query string, paths []string, limit int) ([]qdrant.Hit, error) {
	return x.search(ctx, projectID, query, paths, KindImage, nil, limit)
}

func stillDimensions(data []byte) (int, int) {
	switch {
	case len(data) >= 24 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return int(binary.BigEndian.Uint32(data[16:20])), int(binary.BigEndian.Uint32(data[20:24]))
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return jpegDimensions(data)
	default:
		return 0, 0
	}
}

func jpegDimensions(data []byte) (int, int) {
	for i := 2; i < len(data)-8; {
		if data[i] != 0xff {
			return 0, 0
		}
		marker := data[i+1]
		if marker == 0xd8 || marker == 0xd9 {
			i += 2
			continue
		}
		if i+3 >= len(data) {
			return 0, 0
		}
		size := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if size < 2 || i+2+size > len(data) {
			return 0, 0
		}
		if marker >= 0xc0 && marker <= 0xc3 {
			if i+8 >= len(data) {
				return 0, 0
			}
			return int(binary.BigEndian.Uint16(data[i+7 : i+9])), int(binary.BigEndian.Uint16(data[i+5 : i+7]))
		}
		i += 2 + size
	}
	return 0, 0
}
