package projects

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
)

const diskSlackBytes = 64 << 20

// EnsureDiskSpace returns an error when path's filesystem cannot hold need bytes.
// A zero or negative need still requires a small slack so tiny uploads do not
// fill the disk. If the filesystem cannot be queried, it returns nil.
func EnsureDiskSpace(path string, need int64) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if need < 0 {
		need = 0
	}
	need += diskSlackBytes
	free, err := freeBytes(path)
	if err != nil {
		return nil
	}
	if free < need {
		return fmt.Errorf("not enough disk space (%s free, need about %s)", formatByteSize(free), formatByteSize(need))
	}
	return nil
}

// IsNoSpace reports whether err is an out-of-disk-space failure.
func IsNoSpace(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ENOSPC) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no space left") || strings.Contains(msg, "not enough disk space")
}

func freeBytes(path string) (int64, error) {
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			return 0, err
		}
		path = "."
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func formatByteSize(n int64) string {
	if n < 0 {
		n = 0
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
