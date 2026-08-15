package ffmpeg

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// Kind is the binary the agent is allowed to invoke.
type Kind string

const (
	KindFFmpeg  Kind = "ffmpeg"
	KindFFprobe Kind = "ffprobe"
)

// Command is a validated argv ready for exec (without the binary).
type Command struct {
	Kind Kind
	Args []string
}

// ValidateOpts controls which input sources are allowed.
type ValidateOpts struct {
	Workspace    string
	AllowNetwork bool
}

// Normalize strips a leading ffmpeg/ffprobe token if present.
func Normalize(tokens []string) (Kind, []string, error) {
	if len(tokens) == 0 {
		return "", nil, fmt.Errorf("empty argv")
	}
	kind, ok := classifyBinary(tokens[0])
	if ok {
		return kind, append([]string(nil), tokens[1:]...), nil
	}
	if strings.HasPrefix(tokens[0], "-") {
		return KindFFmpeg, append([]string(nil), tokens...), nil
	}
	return "", nil, fmt.Errorf("command must start with ffmpeg, ffprobe, or a flag; got %q", tokens[0])
}

func classifyBinary(first string) (Kind, bool) {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(first)))
	base = strings.TrimSuffix(base, ".exe")
	switch base {
	case "ffmpeg":
		return KindFFmpeg, true
	case "ffprobe":
		return KindFFprobe, true
	}
	return "", false
}

// Validate checks argv for sandbox violations and returns a Command.
func Validate(tokens []string, opts ValidateOpts) (Command, error) {
	kind, args, err := Normalize(tokens)
	if err != nil {
		return Command{}, err
	}
	workspace, err := filepath.Abs(opts.Workspace)
	if err != nil {
		return Command{}, err
	}

	lastFormat := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "" {
			return Command{}, fmt.Errorf("empty argument at position %d", i)
		}
		if looksLikeFlag(arg) {
			name, inline := splitFlag(arg)
			switch name {
			case "-f", "-format":
				if inline != "" {
					lastFormat = inline
				} else if i+1 < len(args) && !looksLikeFlag(args[i+1]) {
					lastFormat = args[i+1]
				}
			case "-i", "-attach", "-filter_script", "-filter_complex_script":
				val := inline
				if val == "" {
					if i+1 >= len(args) {
						return Command{}, fmt.Errorf("%s requires a value", name)
					}
					i++
					val = args[i]
				}
				if err := validateInput(val, lastFormat, workspace, opts.AllowNetwork); err != nil {
					return Command{}, err
				}
				lastFormat = ""
			case "-filter", "-filter_complex", "-vf", "-af":
				val := inline
				if val == "" {
					if i+1 >= len(args) {
						return Command{}, fmt.Errorf("%s requires a value", name)
					}
					i++
					val = args[i]
				}
				if err := validateFilterGraph(val, workspace); err != nil {
					return Command{}, err
				}
			}
			continue
		}

		// Positional (typically the output file). Last positional is rewritten
		// as a workspace path; earlier ones are treated as inputs.
		if isLavfiExpr(arg) {
			continue
		}
		if err := validatePath(arg, workspace, opts.AllowNetwork); err != nil {
			return Command{}, err
		}
	}

	return Command{Kind: kind, Args: args}, nil
}

func looksLikeFlag(s string) bool {
	return strings.HasPrefix(s, "-") && s != "-"
}

func splitFlag(arg string) (name, inline string) {
	if i := strings.IndexByte(arg, '='); i >= 0 {
		return arg[:i], arg[i+1:]
	}
	return arg, ""
}

func validateInput(val, format, workspace string, allowNet bool) error {
	if format == "lavfi" || isLavfiExpr(val) {
		return nil
	}
	return validatePath(val, workspace, allowNet)
}

func validatePath(val, workspace string, allowNet bool) error {
	lower := strings.ToLower(val)
	switch {
	case strings.HasPrefix(lower, "pipe:"), val == "-":
		return fmt.Errorf("pipe inputs/outputs are not allowed")
	case strings.Contains(lower, "://"):
		u, err := url.Parse(val)
		if err != nil {
			return fmt.Errorf("invalid URL %q", val)
		}
		if !allowNet {
			return fmt.Errorf("network input %q is disabled", val)
		}
		switch u.Scheme {
		case "http", "https", "rtmp", "rtmps", "rtsp", "rtsps":
			return nil
		default:
			return fmt.Errorf("unsupported URL scheme %q", u.Scheme)
		}
	}

	abs, err := resolveInWorkspace(workspace, val)
	if err != nil {
		return err
	}
	_ = abs
	return nil
}

func validateFilterGraph(graph, workspace string) error {
	// Pull common filename-bearing filter options out of the graph.
	// Not a full filter parser — a safety net for the usual cases.
	keys := []string{"filename=", "subtitles=", "ass=", "movie=", "fontsdir="}
	lower := strings.ToLower(graph)
	for _, key := range keys {
		search := graph
		lsearch := lower
		for {
			idx := strings.Index(lsearch, key)
			if idx < 0 {
				break
			}
			rest := search[idx+len(key):]
			val := takeFilterValue(rest)
			if val != "" && !isLavfiExpr(val) {
				if err := validatePath(val, workspace, false); err != nil {
					return fmt.Errorf("filter path %q: %w", val, err)
				}
			}
			search = rest
			lsearch = strings.ToLower(rest)
		}
	}
	return nil
}

func takeFilterValue(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	quote := rune(0)
	if s[0] == '\'' || s[0] == '"' {
		quote = rune(s[0])
		s = s[1:]
	}
	var b strings.Builder
	for _, r := range s {
		if quote != 0 {
			if r == quote {
				break
			}
			b.WriteRune(r)
			continue
		}
		if r == ':' || r == ',' || r == ';' || r == '[' || r == ']' || r == ' ' {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isLavfiExpr(s string) bool {
	if s == "" {
		return false
	}
	base := s
	if i := strings.IndexByte(s, '='); i >= 0 {
		base = s[:i]
	}
	switch strings.ToLower(base) {
	case "color", "testsrc", "testsrc2", "smptebars", "smptehdbars",
		"rgbtestsrc", "yuvtestsrc", "nullsrc", "sine", "anoisesrc",
		"anullsrc", "mandelbrot", "life", "cellauto", "gradient",
		"haldclutsrc", "allyuv", "allrgb", "pal75bars", "pal100bars",
		"colorchart":
		return true
	}
	return false
}

func resolveInWorkspace(workspace, p string) (string, error) {
	workspace = filepath.Clean(workspace)
	var full string
	if filepath.IsAbs(p) {
		full = filepath.Clean(p)
	} else {
		full = filepath.Clean(filepath.Join(workspace, p))
	}
	rel, err := filepath.Rel(workspace, full)
	if err != nil {
		return "", fmt.Errorf("path %q is outside the workspace", p)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the workspace", p)
	}
	return full, nil
}

// ResolveInWorkspace is exported for tools that only need a path check.
func ResolveInWorkspace(workspace, p string) (string, error) {
	return resolveInWorkspace(workspace, p)
}
