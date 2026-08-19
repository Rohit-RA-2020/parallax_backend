package projects

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const copyBufSize = 1 << 20

func objectsDir(p Project) string { return filepath.Join(p.Dir, ".parallax", "objects") }

func objectIndexPath(p Project) string {
	return filepath.Join(p.Dir, ".parallax", "object-index.json")
}

type objectIndexEntry struct {
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime_ns"`
	Hash  string `json:"hash"`
}

func snapshotMedia(p Project, previous map[string]string) (map[string]string, error) {
	out := map[string]string{}
	index := readObjectIndex(p)
	changed := false
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
		slash := filepath.ToSlash(rel)
		info, err := entry.Info()
		if err != nil {
			info, err = os.Stat(path)
			if err != nil {
				return err
			}
		}
		if hash, ok := reuseMediaHash(p, path, slash, info, previous, index); ok {
			out[slash] = hash
			return nil
		}
		hash, err := storeMediaObject(p, path)
		if err != nil {
			return err
		}
		out[slash] = hash
		index[slash] = objectIndexEntry{Size: info.Size(), Mtime: info.ModTime().UnixNano(), Hash: hash}
		changed = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if changed {
		_ = writeObjectIndex(p, index)
	}
	return out, nil
}

func reuseMediaHash(p Project, abs, rel string, info os.FileInfo, previous map[string]string, index map[string]objectIndexEntry) (string, bool) {
	if info == nil {
		return "", false
	}
	if entry, ok := index[rel]; ok && entry.Size == info.Size() && entry.Mtime == info.ModTime().UnixNano() && len(entry.Hash) == 64 {
		if _, err := os.Stat(filepath.Join(objectsDir(p), entry.Hash)); err == nil {
			return entry.Hash, true
		}
	}
	if hash := strings.TrimSpace(previous[rel]); len(hash) == 64 {
		dest := filepath.Join(objectsDir(p), hash)
		if sameFile(abs, dest) {
			return hash, true
		}
	}
	return "", false
}

func sameFile(a, b string) bool {
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(sa, sb)
}

func readObjectIndex(p Project) map[string]objectIndexEntry {
	out := map[string]objectIndexEntry{}
	b, err := os.ReadFile(objectIndexPath(p))
	if err != nil {
		return out
	}
	_ = json.Unmarshal(b, &out)
	if out == nil {
		return map[string]objectIndexEntry{}
	}
	return out
}

func writeObjectIndex(p Project, index map[string]objectIndexEntry) error {
	if index == nil {
		index = map[string]objectIndexEntry{}
	}
	dir := filepath.Join(p.Dir, ".parallax")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".object-index-*")
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
	if err := os.Rename(name, objectIndexPath(p)); err != nil {
		return err
	}
	ok = true
	return nil
}

func storeMediaObject(p Project, source string) (string, error) {
	f, err := os.Open(source)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if err := copyStream(h, f); err != nil {
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

func copyStream(dst io.Writer, src io.Reader) error {
	buf := make([]byte, copyBufSize)
	_, err := io.CopyBuffer(dst, src, buf)
	return err
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
	if err := copyStream(tmp, src); err != nil {
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
