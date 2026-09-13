package httpapi

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// providerIconExts are the only file types served under /provider-icons/.
// Provider logos are small raster/vector images; anything else 404s.
var providerIconExts = map[string]bool{
	".png":  true,
	".svg":  true,
	".jpg":  true,
	".jpeg": true,
	".webp": true,
	".gif":  true,
	".ico":  true,
	".avif": true,
}

func (s *Server) providerIconsDir() string {
	if s != nil && strings.TrimSpace(s.ProviderIconsDir) != "" {
		return strings.TrimSpace(s.ProviderIconsDir)
	}
	return "provider-icons"
}

// handleProviderIcons serves LLM provider logos from the backend's
// provider-icons directory (PARALLAX_PROVIDER_ICONS_DIR, default
// ./provider-icons). Paths such as /provider-icons/gemini-dark.png are
// configured via LLM_<ID>_PROVIDER_ICON_* env vars and resolved by the
// frontend against the app origin, which proxies here.
func (s *Server) handleProviderIcons(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, "/provider-icons/")
	rel = strings.TrimSpace(rel)
	if rel == "" || rel == "." || rel == "/" {
		http.NotFound(w, r)
		return
	}
	// URL paths always use forward slashes; reject backslashes and NULs so
	// Windows-style escapes never reach the filesystem join below.
	if strings.Contains(rel, "\\") || strings.ContainsRune(rel, 0) {
		http.NotFound(w, r)
		return
	}
	cleaned := filepath.Clean(rel)
	if cleaned == "." || cleaned == "/" || strings.HasPrefix(cleaned, "..") || filepath.IsAbs(cleaned) {
		http.NotFound(w, r)
		return
	}
	if !providerIconExts[strings.ToLower(filepath.Ext(cleaned))] {
		http.NotFound(w, r)
		return
	}
	base := s.providerIconsDir()
	full := filepath.Join(base, cleaned)
	// Resolve both sides to absolute paths so a relative base (./provider-icons
	// in dev, /app/provider-icons in the container) still blocks escapes.
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	fullAbs, err := filepath.Abs(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	escape, err := filepath.Rel(baseAbs, fullAbs)
	if err != nil || escape == "." || strings.HasPrefix(escape, "..") {
		http.NotFound(w, r)
		return
	}
	st, err := os.Stat(fullAbs)
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeFile(w, r, fullAbs)
}
