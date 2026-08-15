package projects

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func objectsDir(p Project) string { return filepath.Join(p.Dir, ".parallax", "objects") }

func snapshotMedia(p Project) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(p.Dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != p.Dir && (entry.Name() == ".parallax" || entry.Name() == "exports" || entry.Name() == ".scratch") {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || !isMediaName(entry.Name()) {
			return nil
		}
		rel, err := filepath.Rel(p.Dir, path)
		if err != nil {
			return err
		}
		hash, err := storeMediaObject(p, path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = hash
		return nil
	})
	return out, err
}

func storeMediaObject(p Project, source string) (string, error) {
	f, err := os.Open(source)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	hash := hex.EncodeToString(h.Sum(nil))
	dest := filepath.Join(objectsDir(p), hash)
	if _, err := os.Stat(dest); err == nil {
		return hash, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(objectsDir(p), 0o700); err != nil {
		return "", err
	}
	return hash, copyFileAtomic(source, dest, 0o600)
}

func copyFileAtomic(source, dest string, mode os.FileMode) error {
	src, err := os.Open(source)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".restore-*")
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
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := io.Copy(tmp, src); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, dest); err != nil {
		return err
	}
	ok = true
	return nil
}

func restoreMedia(p Project, target, current map[string]string) error {
	for rel := range current {
		if _, keep := target[rel]; !keep && safeHistoryMediaPath(rel) {
			_ = os.Remove(filepath.Join(p.Dir, filepath.FromSlash(rel)))
		}
	}
	for rel, hash := range target {
		if !safeHistoryMediaPath(rel) || len(hash) != 64 {
			continue
		}
		dest := filepath.Join(p.Dir, filepath.FromSlash(rel))
		if current[rel] == hash {
			if _, err := os.Stat(dest); err == nil {
				continue
			}
		}
		if err := copyFileAtomic(filepath.Join(objectsDir(p), hash), dest, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func safeHistoryMediaPath(rel string) bool {
	rel = filepath.Clean(filepath.FromSlash(rel))
	return rel != "." && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !strings.HasPrefix(rel, ".parallax"+string(filepath.Separator)) && !strings.HasPrefix(rel, "exports"+string(filepath.Separator))
}
