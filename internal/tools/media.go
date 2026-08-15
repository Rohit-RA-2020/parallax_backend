package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"parallax/internal/ffmpeg"
	"parallax/internal/llm"
)

// MediaEnv is the execution context shared by ffmpeg-backed tools.
type MediaEnv struct {
	Workspace string
	Bins      ffmpeg.Bins
	AllowNet  bool
}

const defaultFFmpegTimeout = 5 * time.Minute
const maxFFmpegTimeout = 30 * time.Minute

func RegisterMedia(reg *Registry, env MediaEnv) {
	reg.Register(llm.NewFunctionTool(
		"list_workspace",
		"List media and subtitle files in the workspace the agent can read and write. Call this before inventing filenames.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"subdir": {"type": "string", "description": "Optional subdirectory relative to the workspace"},
				"glob":   {"type": "string", "description": "Optional glob such as *.mp4 or **/*.srt"}
			}
		}`),
	), env.listWorkspace)

	reg.Register(llm.NewFunctionTool(
		"inspect_file",
		"Stat a workspace file (size, extension, modified time). Use probe_media for streams and codecs.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Path relative to the workspace"}
			},
			"required": ["path"]
		}`),
	), env.inspectFile)

	reg.Register(llm.NewFunctionTool(
		"probe_media",
		"Run ffprobe and return JSON stream/format metadata for a workspace media file. Always probe before writing an ffmpeg command against a file you have not inspected.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Path relative to the workspace"}
			},
			"required": ["path"]
		}`),
	), env.probeMedia)

	reg.Register(llm.NewFunctionTool(
		"run_ffmpeg",
		"Execute one validated ffmpeg or ffprobe command inside the workspace sandbox. Prefer the args array (no binary name). The command string form is accepted as a fallback and is parsed without a shell. All input and output paths must stay inside the workspace. On failure, read stderr and fix the command.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"rationale": {
					"type": "string",
					"description": "One or two sentences explaining what this command does and why."
				},
				"args": {
					"type": "array",
					"items": {"type": "string"},
					"description": "Argv WITHOUT the binary. Example: [\"-y\",\"-i\",\"in.mp4\",\"-c:v\",\"copy\",\"-an\",\"out.mp4\"]"
				},
				"command": {
					"type": "string",
					"description": "Full command beginning with ffmpeg or ffprobe. Used only when args is empty. No pipes, redirections, or shell syntax."
				},
				"timeout_seconds": {
					"type": "integer",
					"description": "Optional timeout, default 300, max 1800."
				}
			},
			"required": ["rationale"]
		}`),
	), env.runFFmpeg)
}

func (e MediaEnv) listWorkspace(_ context.Context, raw json.RawMessage) Result {
	var in struct {
		Subdir string `json:"subdir"`
		Glob   string `json:"glob"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{OK: false, Error: err.Error()}
	}

	root := e.Workspace
	if strings.TrimSpace(in.Subdir) != "" {
		resolved, err := ffmpeg.ResolveInWorkspace(e.Workspace, in.Subdir)
		if err != nil {
			return Result{OK: false, Error: err.Error()}
		}
		root = resolved
	}

	pattern := strings.TrimSpace(in.Glob)
	var files []map[string]any
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") && path != root {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(e.Workspace, path)
		if err != nil {
			return err
		}
		if pattern != "" {
			baseMatch, _ := filepath.Match(pattern, name)
			relMatch, _ := filepath.Match(pattern, rel)
			if !baseMatch && !relMatch {
				return nil
			}
		} else if !isMediaName(name) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files = append(files, map[string]any{
			"path":  filepath.ToSlash(rel),
			"bytes": info.Size(),
			"ext":   strings.ToLower(filepath.Ext(name)),
		})
		if len(files) >= 200 {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if files == nil {
		files = []map[string]any{}
	}
	return Result{OK: true, Output: map[string]any{
		"workspace": e.Workspace,
		"count":     len(files),
		"files":     files,
	}}
}

func (e MediaEnv) inspectFile(_ context.Context, raw json.RawMessage) Result {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if strings.TrimSpace(in.Path) == "" {
		return Result{OK: false, Error: "path is required"}
	}
	abs, err := ffmpeg.ResolveInWorkspace(e.Workspace, in.Path)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	rel, _ := filepath.Rel(e.Workspace, abs)
	return Result{OK: true, Output: map[string]any{
		"path":     filepath.ToSlash(rel),
		"bytes":    info.Size(),
		"dir":      info.IsDir(),
		"modified": info.ModTime().UTC().Format(time.RFC3339),
		"ext":      strings.ToLower(filepath.Ext(info.Name())),
	}}
}

func (e MediaEnv) probeMedia(ctx context.Context, raw json.RawMessage) Result {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if strings.TrimSpace(in.Path) == "" {
		return Result{OK: false, Error: "path is required"}
	}
	if _, err := ffmpeg.ResolveInWorkspace(e.Workspace, in.Path); err != nil {
		return Result{OK: false, Error: err.Error()}
	}

	cmd, err := ffmpeg.Validate([]string{
		"ffprobe",
		"-v", "error",
		"-show_format",
		"-show_streams",
		"-print_format", "json",
		in.Path,
	}, ffmpeg.ValidateOpts{Workspace: e.Workspace, AllowNetwork: e.AllowNet})
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}

	res, err := ffmpeg.Run(ctx, e.Bins, cmd, e.Workspace, 30*time.Second)
	if err != nil {
		return Result{OK: false, Error: err.Error(), Output: map[string]any{
			"stderr": trimOutput(res.Stderr, 8<<10),
		}}
	}

	var parsed any
	if json.Unmarshal([]byte(res.Stdout), &parsed) != nil {
		return Result{OK: true, Output: map[string]any{"raw": trimOutput(res.Stdout, 16<<10)}}
	}
	return Result{OK: true, Output: parsed}
}

func (e MediaEnv) runFFmpeg(ctx context.Context, raw json.RawMessage) Result {
	var in struct {
		Rationale      string   `json:"rationale"`
		Args           []string `json:"args"`
		Command        string   `json:"command"`
		TimeoutSeconds int      `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if strings.TrimSpace(in.Rationale) == "" {
		return Result{OK: false, Error: "rationale is required so the command intent is structured"}
	}

	tokens := in.Args
	if len(tokens) == 0 {
		if strings.TrimSpace(in.Command) == "" {
			return Result{OK: false, Error: "provide args (preferred) or command"}
		}
		parsed, err := ffmpeg.Tokenize(in.Command)
		if err != nil {
			return Result{OK: false, Error: err.Error()}
		}
		tokens = parsed
	}

	cmd, err := ffmpeg.Validate(tokens, ffmpeg.ValidateOpts{
		Workspace:    e.Workspace,
		AllowNetwork: e.AllowNet,
	})
	if err != nil {
		return Result{OK: false, Error: "invalid ffmpeg command: " + err.Error()}
	}

	timeout := defaultFFmpegTimeout
	if in.TimeoutSeconds > 0 {
		timeout = time.Duration(in.TimeoutSeconds) * time.Second
		if timeout > maxFFmpegTimeout {
			timeout = maxFFmpegTimeout
		}
	}

	res, err := ffmpeg.Run(ctx, e.Bins, cmd, e.Workspace, timeout)
	out := map[string]any{
		"rationale": in.Rationale,
		"kind":      res.Kind,
		"args":      res.Args,
		"exit_code": res.ExitCode,
		"duration":  res.Duration,
		"stdout":    trimOutput(res.Stdout, 8<<10),
		"stderr":    trimOutput(res.Stderr, 12<<10),
	}
	if err != nil {
		return Result{OK: false, Error: err.Error(), Output: out}
	}
	return Result{OK: true, Output: out}
}

func isMediaName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4", ".mov", ".mkv", ".webm", ".avi", ".m4v", ".ts", ".mts",
		".mp3", ".wav", ".aac", ".flac", ".m4a", ".ogg", ".opus",
		".jpg", ".jpeg", ".png", ".webp", ".gif", ".bmp", ".tif", ".tiff",
		".srt", ".ass", ".ssa", ".vtt", ".lrc":
		return true
	}
	return false
}

func trimOutput(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n… truncated %d bytes …", len(s)-n)
}
