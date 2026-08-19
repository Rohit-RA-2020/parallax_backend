package ffmpeg

import (
	"path/filepath"
	"strings"
)

// MediaIO is the set of workspace file paths an ffmpeg argv reads and writes.
type MediaIO struct {
	Inputs  []string
	Outputs []string
}

// ParseMediaIO extracts file inputs (-i) and the last positional output path.
func ParseMediaIO(args []string) MediaIO {
	var io MediaIO
	for i := 0; i < len(args); i++ {
		name, inline := splitFlag(args[i])
		if name != "-i" && name != "-attach" {
			continue
		}
		val := inline
		if val == "" && i+1 < len(args) {
			i++
			val = args[i]
		}
		if isWorkspaceMediaPath(val) {
			io.Inputs = append(io.Inputs, filepath.ToSlash(val))
		}
	}
	if n := len(args); n > 0 && !looksLikeFlag(args[n-1]) && isWorkspaceMediaPath(args[n-1]) {
		io.Outputs = []string{filepath.ToSlash(args[n-1])}
	}
	return io
}

// RewriteOutput replaces the last positional output, or appends one.
func RewriteOutput(args []string, output string) []string {
	out := append([]string(nil), args...)
	if len(out) > 0 && !looksLikeFlag(out[len(out)-1]) && isWorkspaceMediaPath(out[len(out)-1]) {
		out[len(out)-1] = output
		return out
	}
	return append(out, output)
}

func isWorkspaceMediaPath(val string) bool {
	val = strings.TrimSpace(val)
	if val == "" || val == "-" || isLavfiExpr(val) || hasProtocol(val) {
		return false
	}
	lower := strings.ToLower(val)
	if strings.HasPrefix(lower, "pipe:") {
		return false
	}
	return true
}

// HasVideoExt is true for container extensions that may carry a picture stream.
func HasVideoExt(rel string) bool {
	return mediaKind(rel) == "video"
}

// SameMediaKind reports whether two paths are the same media family.
func SameMediaKind(a, b string) bool {
	ka, kb := mediaKind(a), mediaKind(b)
	return ka != "" && ka == kb
}

func mediaKind(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".mov", ".mkv", ".webm", ".avi", ".m4v", ".ts", ".mts":
		return "video"
	case ".mp3", ".wav", ".aac", ".flac", ".m4a", ".ogg", ".opus":
		return "audio"
	case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".bmp", ".tif", ".tiff":
		return "image"
	case ".srt", ".ass", ".ssa", ".vtt", ".lrc":
		return "subtitle"
	default:
		return ""
	}
}

// LooksLikeDerivative is true when output is the same stem or a suffixed
// sibling of input (talk_muted.mp4 from talk.mp4). Distinct names stay new files.
func LooksLikeDerivative(input, output string) bool {
	inBase := stem(input)
	outBase := stem(output)
	if inBase == "" || outBase == "" {
		return false
	}
	if strings.EqualFold(inBase, outBase) {
		return true
	}
	if len(outBase) <= len(inBase) || !strings.HasPrefix(strings.ToLower(outBase), strings.ToLower(inBase)) {
		return false
	}
	next := outBase[len(inBase)]
	return next == '_' || next == '-' || next == ' ' || next == '.'
}

func stem(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// GenericOutputName is a throwaway render name that should never appear in the bin.
func GenericOutputName(path string) bool {
	switch strings.ToLower(stem(path)) {
	case "out", "output", "tmp", "temp", "result", "processed", "render", "export", "scratch":
		return true
	}
	return strings.HasPrefix(filepath.ToSlash(path), ".scratch/")
}
