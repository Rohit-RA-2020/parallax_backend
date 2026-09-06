// Package tools — Blender headless integration for the Director agent.
//
// Architecture: the Director speaks the Blender MCP bridge protocol
// (newline-delimited JSON over TCP, default 127.0.0.1:9876) defined by the
// djeada/blender-mcp-server addon. When the bridge is reachable (GUI Blender
// with the addon, or scripts/blender-headless-bridge.py running under
// `blender --background`), commands go there. Otherwise blender_run_script
// and blender_render fall back to spawning `blender --background --python`
// directly — no display, no daemon, same power for scene creation + renders.
//
// All file outputs are confined to the project workspace (media/); renders
// are indexed into the bin via OnApplied like generate_image.
package tools

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"parallax/internal/llm"
)

const (
	defaultBlenderBridgeHost = "127.0.0.1"
	defaultBlenderBridgePort = 9876
	defaultBlenderTimeout    = 10 * time.Minute
	maxBlenderCodeRunes      = 20000
	maxBlenderOutput         = 20000
	maxBlenderBridgeBytes    = 16 << 20 // cap a single bridge response line
)

// blenderHeadlessSem caps concurrent `blender --background` processes so
// parallel Director calls cannot OOM the container. EEVEE stills are
// ~0.5-1GB RSS each; 2 at a time is safe on small hosts.
var blenderHeadlessSem = make(chan struct{}, 2)

// BlenderEnv configures Director access to headless Blender.
// Bins are resolved from BLENDER_BIN (default "blender"); the bridge address
// comes from BLENDER_BRIDGE_HOST / BLENDER_BRIDGE_PORT.
type BlenderEnv struct {
	Workspace  string
	BlenderBin string
	BridgeHost string
	BridgePort int
	Timeout    time.Duration
	OnMutation func()
	OnApplied  func(rel string)
}

// RegisterBlender exposes three Director tools:
//   - blender_scene_info: inspect the live bridge scene (objects, engine, resolution).
//   - blender_run_script: execute bpy Python via bridge, else headless fallback.
//   - blender_render:    build a scene from bpy code headless and render a
//     still (PNG) or animation (MP4) into media/ as a bin asset.
func RegisterBlender(reg *Registry, env BlenderEnv) {
	reg.Register(llm.NewFunctionTool(
		"blender_scene_info",
		"Inspect the live Blender MCP bridge scene (name, engine, resolution, object count, object list). Requires Blender running with the MCP Bridge addon (GUI) or scripts/blender-headless-bridge.py. Use before creating objects in a live session.",
		json.RawMessage(`{
			"type":"object",
			"properties":{
				"type":{"type":"string","description":"Optional object type filter such as MESH, CAMERA, LIGHT, EMPTY"}
			}
		}`),
	), env.sceneInfo)

	reg.Register(llm.NewFunctionTool(
		"blender_run_script",
		"Execute Blender Python (bpy) through the MCP bridge (python.execute), falling back to headless blender --background when the bridge is down. Use for scene modeling, materials, lighting, animation setup. Set __result__ in code to return structured JSON. Never import subprocess, socket, os.system, or touch files outside the workspace.",
		json.RawMessage(`{
			"type":"object",
			"properties":{
				"code":{"type":"string","description":"Blender Python code. bpy and mathutils are pre-imported in headless mode; set __result__ to return JSON."},
				"timeout_seconds":{"type":"integer","minimum":10,"maximum":1800,"description":"Execution timeout, default 300"}
			},
			"required":["code"]
		}`),
	), env.runScript)

	reg.Register(llm.NewFunctionTool(
		"blender_render",
		"Create a Blender scene from bpy code and render it headless (BLENDER_EEVEE, CPU) into media/ as a project asset. The code must build the scene (objects, materials, lights, camera) — a camera is auto-created when missing. Returns the workspace path; the file is indexed into the bin automatically. Call place_media afterward only when the user asked to put it on the timeline.",
		json.RawMessage(`{
			"type":"object",
			"properties":{
				"code":{"type":"string","description":"Blender Python scene code (bpy pre-imported). Build objects/materials/lights/camera here."},
				"filename":{"type":"string","description":"Output basename under media/, e.g. neon-alley.png or flythrough.mp4. Defaults to blender-render.<ext>"},
				"width":{"type":"integer","minimum":64,"maximum":4096,"description":"Render width, default 1280"},
				"height":{"type":"integer","minimum":64,"maximum":4096,"description":"Render height, default 720"},
				"frames":{"type":"integer","minimum":1,"maximum":600,"description":"1 = still PNG; >1 = MP4 animation of this many frames starting at frame 1"},
				"engine":{"type":"string","enum":["EEVEE","CYCLES"],"description":"Render engine, default EEVEE (fast, CPU). CYCLES is slower but higher fidelity."},
				"timeout_seconds":{"type":"integer","minimum":30,"maximum":3600,"description":"Render timeout, default 600"}
			},
			"required":["code"]
		}`),
	), env.render)
}

func (e BlenderEnv) blenderBin() string {
	if strings.TrimSpace(e.BlenderBin) != "" {
		return strings.TrimSpace(e.BlenderBin)
	}
	return "blender"
}

func (e BlenderEnv) bridgeHost() string {
	if strings.TrimSpace(e.BridgeHost) != "" {
		return strings.TrimSpace(e.BridgeHost)
	}
	return defaultBlenderBridgeHost
}

func (e BlenderEnv) bridgePort() int {
	if e.BridgePort > 0 {
		return e.BridgePort
	}
	return defaultBlenderBridgePort
}

func (e BlenderEnv) timeout(d time.Duration) time.Duration {
	if e.Timeout > 0 {
		return e.Timeout
	}
	if d > 0 {
		return d
	}
	return defaultBlenderTimeout
}

// effectiveTimeout resolves a per-call timeout_seconds against the env
// default (BLENDER_TIMEOUT_SECONDS). Unset requests fall back to the env
// value (or the hardcoded default); explicit requests are honored up to
// hardMax so the env var is never dead config.
func (e BlenderEnv) effectiveTimeout(reqSeconds int, def, hardMax time.Duration) time.Duration {
	if reqSeconds <= 0 {
		return e.timeout(def)
	}
	t := time.Duration(reqSeconds) * time.Second
	if t > hardMax {
		t = hardMax
	}
	return t
}

// validateBlenderCode is a best-effort static guardrail. Blender Python runs
// with full interpreter power, so the prompt alone ("never import ...") is
// not enforcement. This rejects the dangerous patterns that would escape
// the workspace sandbox (process spawn, network, destructive file ops,
// nested exec/eval that could bypass these checks, .blend file writes).
func validateBlenderCode(code string) error {
	if len([]rune(code)) > maxBlenderCodeRunes {
		return fmt.Errorf("code must be at most %d characters", maxBlenderCodeRunes)
	}
	lowered := strings.ToLower(code)
	// 1. Blocked module imports: parse `import x` / `from x import`.
	blockedMods := map[string]bool{
		"subprocess": true, "multiprocessing": true, "socket": true,
		"urllib": true, "urllib3": true, "requests": true, "httpx": true,
		"aiohttp": true, "http": true, "ftplib": true, "smtplib": true,
		"telnetlib": true, "ssl": true, "paramiko": true, "ctypes": true,
		"cffi": true, "importlib": true, "pkgutil": true, "sys": true,
		"venv": true, "pip": true, "ensurepip": true, "webbrowser": true,
		"pydoc": true, "pty": true, "shlex": true,
	}
	importRe := regexp.MustCompile(`(?m)^\s*(?:import|from)\s+([a-zA-Z0-9_\.]+)`)
	for _, m := range importRe.FindAllStringSubmatch(lowered, -1) {
		base := strings.Split(m[1], ".")[0]
		if blockedMods[base] {
			return fmt.Errorf("import %q is not allowed in Blender code (workspace sandbox)", m[1])
		}
	}
	// 2. Blocked call patterns (word-boundary aware).
	blockedRes := []string{
		`\bos\.system\s*\(`, `\bos\.popen\s*\(`, `\bos\.exec\w*\s*\(`, `\bos\.spawn\w*\s*\(`, 
		`\bos\.fork\s*\(`, `\bos\.kill\s*\(`, `\bos\.remove\s*\(`, `\bos\.unlink\s*\(`, 
		`\bos\.rmdir\s*\(`, `\bos\.removedirs\s*\(`, `\bos\.rename\s*\(`, `\bos\.replace\s*\(`, 
		`\bos\.truncate\s*\(`, `\bos\.chmod\s*\(`, `\bos\.chown\s*\(`, `\bos\.symlink\s*\(`, 
		`\bos\.link\s*\(`, `\bos\.makedirs\s*\(`, `\bos\.mkdir\s*\(`, `\bos\.putenv\s*\(`, 
		`\bshutil\s*\.\s*\w+`, `\bpathlib\s*\.\s*\w+`, `\b__import__\s*\(`, `\beval\s*\(`, 
		`\bexec\s*\(`, `\bcompile\s*\(`, `\bopen\s*\(`, `\binput\s*\(`, `\bbreakpoint\s*\(`, 
		`\bsave_as_mainfile\b`, `\bsave_mainfile\b`, `\bopen_mainfile\b`,
	}
	for _, pat := range blockedRes {
		if matched, _ := regexp.MatchString(pat, lowered); matched {
			return fmt.Errorf("pattern %q is not allowed in Blender code (workspace sandbox; file output is handled by blender_render)", strings.Trim(pat, `\b`))
		}
	}
	// 3. Absolute system paths in string literals.
	absPathRe := regexp.MustCompile(`['"]/((tmp|etc|home|root|usr|var|opt|proc|sys|dev)(/|"))`)
	if absPathRe.MatchString(code) {
		return fmt.Errorf("absolute system paths are not allowed in Blender code (stay inside the workspace)")
	}
	return nil
}

// bridgeRequest speaks one MCP-bridge command: {"id","command","params"}\n
// and parses {"id","success","result","error"}\n.
// Each request uses a fresh TCP connection; the response id is verified and
// the response line is size-capped so a huge scene listing cannot OOM us.
func (e BlenderEnv) bridgeRequest(ctx context.Context, command string, params map[string]any) (any, error) {
	addr := fmt.Sprintf("%s:%d", e.bridgeHost(), e.bridgePort())
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("blender bridge at %s is not reachable (start Blender with the MCP Bridge addon or run scripts/blender-headless-bridge.py): %w", addr, err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(e.timeout(0)))
	}
	id := newID()
	req := map[string]any{"id": id, "command": command, "params": params}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("reading blender bridge response: %w", err)
	}
	if len(line) > maxBlenderBridgeBytes {
		return nil, fmt.Errorf("blender bridge response too large (%d bytes, cap %d)", len(line), maxBlenderBridgeBytes)
	}
	var resp struct {
		ID      string `json:"id"`
		Success bool   `json:"success"`
		Result  any    `json:"result"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("invalid blender bridge response: %w", err)
	}
	if resp.ID != "" && resp.ID != id {
		return nil, fmt.Errorf("blender bridge response id mismatch (want %s got %s)", id, resp.ID)
	}
	if !resp.Success {
		if resp.Error == "" {
			resp.Error = "unknown blender error"
		}
		return nil, fmt.Errorf("blender %s failed: %s", command, resp.Error)
	}
	return resp.Result, nil
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func (e BlenderEnv) sceneInfo(ctx context.Context, raw json.RawMessage) Result {
	var in struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &in)
	ctx, cancel := context.WithTimeout(ctx, e.timeout(60*time.Second))
	defer cancel()
	info, err := e.bridgeRequest(ctx, "scene.get_info", nil)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	params := map[string]any{}
	if t := strings.ToUpper(strings.TrimSpace(in.Type)); t != "" {
		params["type"] = t
	}
	objects, err := e.bridgeRequest(ctx, "scene.list_objects", params)
	if err != nil {
		return Result{OK: true, Output: map[string]any{"scene": info, "objects_error": err.Error()}}
	}
	return Result{OK: true, Output: map[string]any{"scene": info, "objects": objects}}
}

func (e BlenderEnv) runScript(ctx context.Context, raw json.RawMessage) Result {
	var in struct {
		Code           string `json:"code"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if strings.TrimSpace(in.Code) == "" {
		return Result{OK: false, Error: "code is required"}
	}
	if err := validateBlenderCode(in.Code); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	timeout := e.effectiveTimeout(in.TimeoutSeconds, 5*time.Minute, 30*time.Minute)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Prefer the live bridge (same transport the MCP server uses).
	if res, err := e.bridgeRequest(ctx, "python.execute", map[string]any{
		"code": in.Code, "timeout_seconds": int(timeout.Seconds()),
	}); err == nil {
		return Result{OK: true, Output: map[string]any{"transport": "bridge", "result": res}}
	} else if ctx.Err() != nil {
		return Result{OK: false, Error: ctx.Err().Error()}
	} else {
		bridgeErr := err.Error()
		// Fall back to headless `blender --background`.
		out, herr := e.execHeadless(ctx, in.Code, "")
		if herr != nil {
			return Result{OK: false, Error: fmt.Sprintf("%s; headless fallback also failed: %s", bridgeErr, herr.Error())}
		}
		out["transport"] = "headless"
		out["bridge_error"] = bridgeErr
		return Result{OK: true, Output: out}
	}
}

// execHeadless runs bpy code in a separate `blender --background --python`
// process. Extra file-writing prelude (e.g. render setup) can be appended via
// suffix. Structured data comes back through the __result__ variable, framed
// by a per-invocation random prefix (anti-spoof: user prints cannot fake it).
func (e BlenderEnv) execHeadless(ctx context.Context, code, suffix string) (map[string]any, error) {
	if strings.TrimSpace(e.Workspace) == "" {
		return nil, fmt.Errorf("workspace is not configured")
	}
	if _, err := exec.LookPath(e.blenderBin()); err != nil {
		// Absolute BLENDER_BIN (e.g. /opt/blender/blender) skips PATH lookup.
		if st, serr := os.Stat(e.blenderBin()); serr != nil || st.IsDir() {
			return nil, fmt.Errorf("blender binary %q not found (set BLENDER_BIN): %v", e.blenderBin(), err)
		}
	}
	// Cap concurrent Blender processes; respect caller cancellation while waiting.
	select {
	case blenderHeadlessSem <- struct{}{}:
		defer func() { <-blenderHeadlessSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	tmp, err := os.MkdirTemp("", "parallax-blender-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	codePath := filepath.Join(tmp, "scene.py")
	argsPath := filepath.Join(tmp, "args.json")
	if err := os.WriteFile(argsPath, []byte(`{}`), 0o600); err != nil {
		return nil, err
	}
	prefix := "__PARALLAX_BLENDER_" + newID() + "__="
	wrapped := headlessWrapper(codePath, argsPath, code, suffix, prefix)
	scriptPath := filepath.Join(tmp, "wrapper.py")
	if err := os.WriteFile(scriptPath, []byte(wrapped), 0o600); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, e.blenderBin(), "--background", "--python", scriptPath)
	cmd.Dir = e.Workspace
	// Minimal deterministic env: keep PATH/HOME/TMPDIR, drop secrets?
	// Blender needs HOME for config; wholesale env scrub breaks GPU/ocio.
	// We keep the process env (container isolation is the boundary).
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("blender headless failed: %v\n%s", err, tailLines(stderr.String()+stdout.String(), 30))
	}
	payload, cleaned := extractHeadlessPayload(stdout.String(), prefix)
	out := map[string]any{"stdout": capText(cleaned, maxBlenderOutput), "stderr": capText(stderr.String(), 4000)}
	if payload == nil {
		return nil, fmt.Errorf("blender returned no result payload\n%s", tailLines(cleaned+stderr.String(), 30))
	}
	out["result"] = payload["result"]
	if msg, _ := payload["error"].(string); msg != "" {
		return nil, fmt.Errorf("blender script error: %s", capText(msg, maxBlenderOutput))
	}
	return out, nil
}

func headlessWrapper(codePath, argsPath, code, suffix, prefix string) string {
	// Code travels via file (not argv) so quoting can never break.
	// Suffix runs after user code for render/output configuration.
	joined := code + "\n" + suffix
	q := func(s string) string {
		b, _ := json.Marshal(s)
		return string(b)
	}
	return "import io, json, pathlib, traceback\n" +
		"from contextlib import redirect_stdout, redirect_stderr\n" +
		"import bpy, mathutils\n" +
		"pathlib.Path(" + q(codePath) + ").write_text(" + q(joined) + ", encoding='utf-8')\n" +
		"code = pathlib.Path(" + q(codePath) + ").read_text(encoding='utf-8')\n" +
		"args = json.loads(pathlib.Path(" + q(argsPath) + ").read_text(encoding='utf-8'))\n" +
		"ns = {'bpy': bpy, 'mathutils': mathutils, 'args': args, '__result__': None}\n" +
		"out_buf, err_buf = io.StringIO(), io.StringIO()\n" +
		"try:\n" +
		"    with redirect_stdout(out_buf), redirect_stderr(err_buf):\n" +
		"        exec(compile(code, '<director-blender>', 'exec'), ns)\n" +
		"    payload = {'result': ns.get('__result__'), 'stdout': out_buf.getvalue(), 'stderr': err_buf.getvalue(), 'error': None}\n" +
		"except Exception as exc:\n" +
		"    tb = ''.join(traceback.format_exception(type(exc), exc, exc.__traceback__)).strip()\n" +
		"    payload = {'result': None, 'stdout': out_buf.getvalue(), 'stderr': err_buf.getvalue(), 'error': tb}\n" +
		"print(" + q(prefix) + " + json.dumps(payload, ensure_ascii=True, default=repr), flush=True)\n"
}

func extractHeadlessPayload(stdout, prefix string) (map[string]any, string) {
	var payload map[string]any
	var clean []string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, prefix) {
			var p map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, prefix)), &p); err == nil {
				payload = p
			}
			continue
		}
		clean = append(clean, line)
	}
	return payload, strings.Join(clean, "\n")
}

func (e BlenderEnv) render(ctx context.Context, raw json.RawMessage) Result {
	var in struct {
		Code           string `json:"code"`
		Filename       string `json:"filename"`
		Width          int    `json:"width"`
		Height         int    `json:"height"`
		Frames         int    `json:"frames"`
		Engine         string `json:"engine"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if strings.TrimSpace(in.Code) == "" {
		return Result{OK: false, Error: "code is required: bpy scene-building code"}
	}
	if err := validateBlenderCode(in.Code); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if strings.TrimSpace(e.Workspace) == "" {
		return Result{OK: false, Error: "workspace is not configured"}
	}
	width, height := in.Width, in.Height
	if width <= 0 {
		width = 1280
	}
	if height <= 0 {
		height = 720
	}
	if width < 64 || height < 64 {
		return Result{OK: false, Error: "resolution below 64px is not allowed"}
	}
	if width > 4096 || height > 4096 {
		return Result{OK: false, Error: "resolution above 4096px is not allowed"}
	}
	frames := in.Frames
	if frames <= 0 {
		frames = 1
	}
	if frames > 600 {
		return Result{OK: false, Error: "at most 600 frames per render"}
	}
	engine := strings.ToUpper(strings.TrimSpace(in.Engine))
	if engine == "" {
		engine = "EEVEE"
	}
	if engine != "EEVEE" && engine != "CYCLES" {
		return Result{OK: false, Error: "engine must be EEVEE or CYCLES"}
	}
	blenderEngine := "BLENDER_EEVEE"
	if engine == "CYCLES" {
		blenderEngine = "CYCLES"
	}
	name := strings.TrimSpace(in.Filename)
	if name == "" {
		name = "blender-render" + defaultRenderExt(frames)
	}
	rel, err := safeMediaRel(e.Workspace, name, frames)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	timeout := e.effectiveTimeout(in.TimeoutSeconds, 10*time.Minute, time.Hour)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	q := func(s string) string {
		b, _ := json.Marshal(s)
		return string(b)
	}
	abs := filepath.Join(e.Workspace, rel)
	// stillFormat follows the requested extension so a .jpg does not get PNG bytes.
	stillFormat := "'PNG'"
	if strings.HasSuffix(strings.ToLower(rel), ".jpg") || strings.HasSuffix(strings.ToLower(rel), ".jpeg") {
		stillFormat = "'JPEG'"
	}
	suffix := "import bpy\n" +
		"scene = bpy.context.scene\n" +
		"scene.render.engine = " + q(blenderEngine) + "\n" +
		"scene.render.resolution_x = " + fmt.Sprint(width) + "\n" +
		"scene.render.resolution_y = " + fmt.Sprint(height) + "\n" +
		"scene.render.resolution_percentage = 100\n" +
		"scene.render.film_transparent = False\n" +
		"try:\n" +
		"    prefs = bpy.context.preferences.addons.get('cycles')\n" +
		"    if prefs is not None and " + q(engine) + " == 'CYCLES':\n" +
		"        scene.cycles.device = 'CPU'\n" +
		"        scene.cycles.samples = min(getattr(scene.cycles, 'samples', 128), 256)\n" +
		"except Exception:\n" +
		"    pass\n" +
		"if scene.camera is None:\n" +
		"    bpy.ops.object.camera_add(location=(7, -7, 5), rotation=(1.1, 0, 0.785))\n" +
		"    scene.camera = bpy.context.active_object\n" +
		"scene.render.filepath = " + q(abs) + "\n"
	if frames == 1 {
		suffix += "scene.render.image_settings.file_format = " + stillFormat + "\n" +
			"scene.render.image_settings.color_mode = 'RGB'\n" +
			"bpy.ops.render.render(write_still=True)\n" +
			"__result__ = {'path': " + q(rel) + ", 'frames': 1, 'engine': " + q(engine) + "}\n"
	} else {
		suffix += "scene.frame_start = 1\n" +
			"scene.frame_end = " + fmt.Sprint(frames) + "\n" +
			"scene.render.image_settings.file_format = 'FFMPEG'\n" +
			"scene.render.image_settings.color_mode = 'RGB'\n" +
			"scene.render.ffmpeg.format = 'MPEG4'\n" +
			"scene.render.ffmpeg.codec = 'H264'\n" +
			"scene.render.ffmpeg.constant_rate_factor = 'MEDIUM'\n" +
			"bpy.ops.render.render(animation=True)\n" +
			"__result__ = {'path': " + q(rel) + ", 'frames': " + fmt.Sprint(frames) + ", 'engine': " + q(engine) + "}\n"
	}
	out, err := e.execHeadless(ctx, in.Code, suffix)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if st, serr := os.Stat(abs); serr != nil || st.Size() == 0 {
		return Result{OK: false, Error: fmt.Sprintf("render finished but %s is missing or empty", rel)}
	}
	if e.OnMutation != nil {
		e.OnMutation()
	}
	if e.OnApplied != nil {
		e.OnApplied(rel)
	}
	out["path"] = rel
	out["engine"] = engine
	return Result{OK: true, Output: out}
}

func defaultRenderExt(frames int) string {
	if frames > 1 {
		return ".mp4"
	}
	return ".png"
}

// safeMediaRel confines the render output to media/ with a sane extension
// and de-duplicates against existing files so repeated renders never
// silently overwrite a previous bin asset (foo.png -> foo-1.png ...).
func safeMediaRel(workspace, name string, frames int) (string, error) {
	base := filepath.Base(strings.TrimSpace(name))
	if base == "" || base == "." || base == ".." || base == "/" {
		return "", fmt.Errorf("filename is required")
	}
	// filepath.Base already strips directories, but reject anything that
	// still looks like traversal or absolute.
	if strings.Contains(base, "\x00") || strings.ContainsAny(base, `/\`) {
		return "", fmt.Errorf("filename must be a plain basename")
	}
	ext := strings.ToLower(filepath.Ext(base))
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if strings.TrimSpace(stem) == "" {
		return "", fmt.Errorf("filename is required")
	}
	if frames > 1 {
		if ext != ".mp4" {
			base = stem + ".mp4"
		}
	} else {
		if ext != ".png" && ext != ".jpg" && ext != ".jpeg" {
			base = stem + ".png"
		}
	}
	if len(base) > 120 {
		return "", fmt.Errorf("filename is too long")
	}
	rel := filepath.Join("media", base)
	abs := filepath.Join(workspace, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	// De-duplicate: keep the requested name when free, else append -N.
	if _, err := os.Stat(abs); os.IsNotExist(err) {
		return rel, nil
	}
	ext2 := filepath.Ext(base)
	stem2 := strings.TrimSuffix(base, ext2)
	for i := 1; i < 1000; i++ {
		cand := filepath.Join("media", stem2+"-"+itoa(i)+ext2)
		if len(filepath.Base(cand)) > 120 {
			return "", fmt.Errorf("filename is too long")
		}
		if _, err := os.Stat(filepath.Join(workspace, cand)); os.IsNotExist(err) {
			return cand, nil
		}
	}
	return "", fmt.Errorf("too many existing renders with that name")
}

func itoa(i int) string { return fmt.Sprint(i) }

func capText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n… (truncated, %d total chars)", len(s))
}

func tailLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
