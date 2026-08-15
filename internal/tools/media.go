package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	Workspace  string
	Bins       ffmpeg.Bins
	AllowNet   bool
	OnMutation func()
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
		"Execute one validated ffmpeg or ffprobe command inside the workspace sandbox. Prefer the args array (no binary name). The command string form is accepted as a fallback and is parsed without a shell. All input and output paths must stay inside the workspace. Transforms of an existing clip are applied back onto that clip (no duplicate bin items). Pass apply_to \"none\" only when the user wants a separate export or a new generated file. On failure, read stderr and fix the command.",
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
					"description": "Argv WITHOUT the binary. Example: [\"-y\",\"-i\",\"media/talk.mp4\",\"-c:v\",\"copy\",\"-an\",\"media/talk_tmp.mp4\"]"
				},
				"command": {
					"type": "string",
					"description": "Full command beginning with ffmpeg or ffprobe. Used only when args is empty. No pipes, redirections, or shell syntax."
				},
				"apply_to": {
					"type": "string",
					"description": "Existing workspace file to update in place after success. Omit to auto-apply when there is a single same-kind source and the output looks like an edit of it. Set to \"none\" to keep a new file (export, highlight, thumbnail, extract)."
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
		ApplyTo        string   `json:"apply_to"`
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

	kind, args, err := ffmpeg.Normalize(tokens)
	if err != nil {
		return Result{OK: false, Error: "invalid ffmpeg command: " + err.Error()}
	}

	applyTo := ""
	if kind != ffmpeg.KindFFprobe {
		var applyErr error
		applyTo, applyErr = resolveApplyTo(e.Workspace, args, in.ApplyTo)
		if applyErr != nil {
			return Result{OK: false, Error: applyErr.Error()}
		}
	}

	scratchRel := ""
	runArgs := args
	if applyTo != "" {
		ext := filepath.Ext(applyTo)
		if ext == "" {
			if io := ffmpeg.ParseMediaIO(args); len(io.Outputs) > 0 {
				ext = filepath.Ext(io.Outputs[0])
			}
			if ext == "" {
				ext = ".mp4"
			}
		}
		scratchRel = filepath.ToSlash(filepath.Join(".scratch", "edit-"+newScratchID()+ext))
		runArgs = ffmpeg.RewriteOutput(args, scratchRel)
	}

	cmd, err := ffmpeg.Validate(runArgs, ffmpeg.ValidateOpts{
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

	if scratchRel != "" {
		if err := os.MkdirAll(filepath.Join(e.Workspace, ".scratch"), 0o755); err != nil {
			return Result{OK: false, Error: err.Error()}
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
		if scratchRel != "" {
			_ = os.Remove(filepath.Join(e.Workspace, filepath.FromSlash(scratchRel)))
		}
		return Result{OK: false, Error: err.Error(), Output: out}
	}

	if applyTo != "" {
		if err := replaceWorkspaceFile(e.Workspace, scratchRel, applyTo); err != nil {
			return Result{OK: false, Error: "applied render failed: " + err.Error(), Output: out}
		}
		out["applied_to"] = applyTo
		out["in_place"] = true
		out["note"] = "Output replaced the existing clip. The project still has one current version of this file."
		if e.OnMutation != nil {
			e.OnMutation()
		}
	} else if io := ffmpeg.ParseMediaIO(args); len(io.Outputs) > 0 {
		out["output"] = io.Outputs[0]
		out["in_place"] = false
		if e.OnMutation != nil {
			e.OnMutation()
		}
	}
	return Result{OK: true, Output: out}
}

func resolveApplyTo(workspace string, args []string, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if strings.EqualFold(raw, "none") || strings.EqualFold(raw, "false") || raw == "-" {
		return "", nil
	}
	if raw != "" {
		abs, err := ffmpeg.ResolveInWorkspace(workspace, raw)
		if err != nil {
			return "", fmt.Errorf("apply_to: %w", err)
		}
		rel, err := filepath.Rel(workspace, abs)
		if err != nil {
			return "", err
		}
		return filepath.ToSlash(rel), nil
	}

	io := ffmpeg.ParseMediaIO(args)
	if len(io.Outputs) != 1 {
		return "", nil
	}
	output := io.Outputs[0]
	var sameKind []string
	for _, in := range io.Inputs {
		if ffmpeg.SameMediaKind(in, output) {
			sameKind = append(sameKind, in)
		}
	}
	if len(sameKind) != 1 {
		return "", nil
	}
	source := sameKind[0]
	if filepath.Clean(source) == filepath.Clean(output) || ffmpeg.LooksLikeDerivative(source, output) || ffmpeg.GenericOutputName(output) {
		abs, err := ffmpeg.ResolveInWorkspace(workspace, source)
		if err != nil {
			return "", err
		}
		rel, err := filepath.Rel(workspace, abs)
		if err != nil {
			return "", err
		}
		return filepath.ToSlash(rel), nil
	}
	return "", nil
}

func replaceWorkspaceFile(workspace, fromRel, toRel string) error {
	fromAbs, err := ffmpeg.ResolveInWorkspace(workspace, fromRel)
	if err != nil {
		return err
	}
	toAbs, err := ffmpeg.ResolveInWorkspace(workspace, toRel)
	if err != nil {
		return err
	}
	if _, err := os.Stat(fromAbs); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(toAbs), 0o755); err != nil {
		return err
	}
	if err := os.Rename(fromAbs, toAbs); err == nil {
		return nil
	}
	src, err := os.Open(fromAbs)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(filepath.Dir(toAbs), ".apply-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := io.Copy(tmp, src); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, toAbs); err != nil {
		return err
	}
	ok = true
	return os.Remove(fromAbs)
}

func newScratchID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
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
