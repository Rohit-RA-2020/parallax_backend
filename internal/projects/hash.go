package projects

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
)

// HashFile returns the sha256 hex digest of a regular file.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if err := copyStream(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
