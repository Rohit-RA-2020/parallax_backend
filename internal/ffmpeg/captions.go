package ffmpeg

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// CaptionFont is a workspace-staged font for burning subtitles.
type CaptionFont struct {
	Name     string
	FontsDir string
	Size     float64
	Fill     string
	Stroke   string
}

// SubtitleFilter builds a sandboxed subtitles= filter that can render the
// requested language. The SRT path must already be inside the workspace.
func SubtitleFilter(workspace, srtRel, language string) (string, error) {
	srtRel = filepath.ToSlash(strings.TrimSpace(srtRel))
	if srtRel == "" {
		return "", fmt.Errorf("subtitle file is required")
	}
	font, err := StageCaptionFont(workspace, language)
	if err != nil {
		return "", err
	}
	return subtitleFilter(srtRel, font), nil
}

func subtitleFilter(srtRel string, font CaptionFont) string {
	size := font.Size
	if size <= 0 {
		size = 32
	}
	primary := assColour(font.Fill, "00FFFFFF")
	outline := assColour(font.Stroke, "00000000")
	style := fmt.Sprintf("Fontsize=%s,Alignment=2,Outline=1.6,Shadow=0.6,MarginV=36,PrimaryColour=&H%s&,OutlineColour=&H%s&", trimFloat(size), primary, outline)
	if font.Name != "" {
		style = "FontName=" + font.Name + "," + style
	}
	filter := "subtitles=" + escapeFilterPath(srtRel)
	if font.FontsDir != "" {
		filter += ":fontsdir=" + escapeFilterPath(font.FontsDir)
	}
	filter += ":force_style=" + escapeFilterPath(style)
	return filter
}

func escapeFilterPath(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `:`, `\:`)
	value = strings.ReplaceAll(value, `'`, `\'`)
	if strings.ContainsAny(value, " ,[]") {
		return "'" + value + "'"
	}
	return value
}

// StageCaptionFont copies a system font that can render language into
// .scratch/fonts so the ffmpeg sandbox can load it.
func StageCaptionFont(workspace, language string) (CaptionFont, error) {
	src, name := pickCaptionFont(language)
	if src == "" {
		return CaptionFont{}, nil
	}
	if strings.TrimSpace(workspace) == "" {
		return CaptionFont{Name: name}, nil
	}
	destDirRel := filepath.ToSlash(filepath.Join(".scratch", "fonts"))
	destDir, err := ResolveInWorkspace(workspace, destDirRel)
	if err != nil {
		return CaptionFont{}, err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return CaptionFont{}, err
	}
	dest := filepath.Join(destDir, filepath.Base(src))
	if err := copyFile(src, dest); err != nil {
		return CaptionFont{Name: name}, nil
	}
	return CaptionFont{Name: name, FontsDir: destDirRel}, nil
}

func assColour(hex, fallback string) string {
	hex = strings.TrimPrefix(strings.TrimSpace(hex), "#")
	if len(hex) == 3 {
		hex = string([]byte{hex[0], hex[0], hex[1], hex[1], hex[2], hex[2]})
	}
	if len(hex) != 6 {
		return fallback
	}
	for i := 0; i < 6; i++ {
		c := hex[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return fallback
		}
	}
	r, g, b := hex[0:2], hex[2:4], hex[4:6]
	return "00" + strings.ToUpper(b+g+r)
}

func trimFloat(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}

func pickCaptionFont(language string) (string, string) {
	lang := strings.ToLower(strings.TrimSpace(language))
	if i := strings.IndexByte(lang, '-'); i > 0 {
		lang = lang[:i]
	}
	type cand struct {
		name string
		path string
	}
	var list []cand
	switch lang {
	case "hi", "mr", "ne", "sa":
		list = []cand{
			{"Noto Sans Devanagari", "/usr/share/fonts/truetype/noto/NotoSansDevanagari-Regular.ttf"},
			{"Noto Serif Devanagari", "/usr/share/fonts/truetype/noto/NotoSerifDevanagari-Regular.ttf"},
		}
	case "ar", "fa", "ur":
		list = []cand{
			{"Noto Naskh Arabic", "/usr/share/fonts/truetype/noto/NotoNaskhArabic-Regular.ttf"},
			{"Noto Sans Arabic", "/usr/share/fonts/truetype/noto/NotoSansArabic-Regular.ttf"},
		}
	case "bn":
		list = []cand{{"Noto Sans Bengali", "/usr/share/fonts/truetype/noto/NotoSansBengali-Regular.ttf"}}
	case "ta":
		list = []cand{{"Noto Sans Tamil", "/usr/share/fonts/truetype/noto/NotoSansTamil-Regular.ttf"}}
	case "te":
		list = []cand{{"Noto Sans Telugu", "/usr/share/fonts/truetype/noto/NotoSansTelugu-Regular.ttf"}}
	case "ja":
		list = []cand{
			{"Noto Sans CJK JP", "/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc"},
			{"Noto Sans CJK JP", "/usr/share/fonts/truetype/noto/NotoSansCJK-Regular.ttc"},
		}
	case "zh":
		list = []cand{
			{"Noto Sans CJK SC", "/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc"},
			{"Noto Sans CJK SC", "/usr/share/fonts/truetype/noto/NotoSansCJK-Regular.ttc"},
		}
	case "ko":
		list = []cand{
			{"Noto Sans CJK KR", "/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc"},
			{"Noto Sans CJK KR", "/usr/share/fonts/truetype/noto/NotoSansCJK-Regular.ttc"},
		}
	}
	list = append(list,
		cand{"Noto Sans", "/usr/share/fonts/truetype/noto/NotoSans-Regular.ttf"},
		cand{"DejaVu Sans", "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"},
	)
	for _, item := range list {
		if _, err := os.Stat(item.path); err == nil {
			return item.path, item.name
		}
	}
	return "", ""
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
