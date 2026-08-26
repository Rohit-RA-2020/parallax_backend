package projects

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

const mediaOriginsFile = "media-origins.json"

func mediaOriginsPath(p Project) string {
	return filepath.Join(p.Dir, ".parallax", mediaOriginsFile)
}

func readMediaOrigins(p Project) map[string]string {
	raw, err := os.ReadFile(mediaOriginsPath(p))
	if err != nil {
		return map[string]string{}
	}
	var origins map[string]string
	if json.Unmarshal(raw, &origins) != nil || origins == nil {
		return map[string]string{}
	}
	return origins
}

func writeMediaOrigins(p Project, origins map[string]string) error {
	raw, err := json.MarshalIndent(origins, "", "  ")
	if err != nil {
		return err
	}
	return writeBytesAtomic(mediaOriginsPath(p), raw, 0o600)
}

// SetMediaOrigin records how a project asset entered the bin. This lets
// timeline placement preserve semantic behavior after reloads.
func (s *Store) SetMediaOrigin(id, rel, origin string) error {
	if _, err := s.ResolveFile(id, rel); err != nil {
		return err
	}
	p, err := s.Get(id)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	origin = strings.ToLower(strings.TrimSpace(origin))
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()
	origins := readMediaOrigins(p)
	if origin == "" {
		delete(origins, rel)
	} else {
		origins[rel] = origin
	}
	return writeMediaOrigins(p, origins)
}

func (s *Store) MediaOrigin(id, rel string) string {
	p, err := s.Get(id)
	if err != nil {
		return ""
	}
	rel = filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	return readMediaOrigins(p)[rel]
}

func LooksLikeGIFImport(path string) bool {
	base := strings.ToLower(filepath.Base(filepath.ToSlash(path)))
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return ext == ".gif" || strings.HasSuffix(stem, "-gif") || strings.Contains(base, ".gif.")
}
