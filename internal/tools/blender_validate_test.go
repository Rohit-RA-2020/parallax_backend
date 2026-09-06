package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeBlenderTestFile(t *testing.T, ws, rel string) {
	t.Helper()
	abs := filepath.Join(ws, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestValidateBlenderCodeAllowsSceneCode(t *testing.T) {
	ok := []string{
		"import bpy\nbpy.ops.mesh.primitive_cube_add(location=(0,0,1))\n__result__ = {'n': 1}",
		"import bpy, mathutils\nmat = bpy.data.materials.new('M')\n__result__ = 1",
		"import bpy\nimport os.path\np = os.path.join('media', 'x.png')",
	}
	for _, c := range ok {
		if err := validateBlenderCode(c); err != nil {
			t.Fatalf("expected allow, got %v for %q", err, c)
		}
	}
}

func TestValidateBlenderCodeBlocks(t *testing.T) {
	blocked := []string{
		"import subprocess\nsubprocess.run(['ls'])",
		"import socket\ns = socket.socket()",
		"import os\nos.system('rm -rf /')",
		"import os\nos.popen('id').read()",
		"import bpy\n__import__('os').system('id')",
		"import bpy\neval('1+1')",
		"import bpy\nexec('x=1')",
		"import bpy\nopen('/etc/passwd').read()",
		"import bpy\nbpy.ops.wm.save_as_mainfile(filepath='/tmp/x.blend')",
		"import bpy\nx = '/etc/passwd'",
		"import shutil\nshutil.rmtree('media')",
		"import sys\nsys.path.insert(0, '/tmp')",
		"import ctypes\nctypes.CDLL('libc.so.6')",
	}
	for _, c := range blocked {
		if err := validateBlenderCode(c); err == nil {
			t.Fatalf("expected block for %q", c)
		}
	}
}

func TestSafeMediaRelTraversalAndDedupe(t *testing.T) {
	ws := t.TempDir()
	rel, err := safeMediaRel(ws, "../../etc/passwd", 1)
	if err != nil {
		t.Fatalf("traversal base should be contained, got err %v", err)
	}
	if !strings.HasPrefix(rel, "media/") {
		t.Fatalf("rel escapes media/: %q", rel)
	}
	if strings.Contains(rel, "..") {
		t.Fatalf("rel contains ..: %q", rel)
	}
	// Absolute input is reduced to basename.
	rel2, err := safeMediaRel(ws, "/tmp/evil.png", 1)
	if err != nil || rel2 != "media/evil.png" {
		t.Fatalf("abs containment: %q %v", rel2, err)
	}
	// Dedupe: second call with same name must not overwrite.
	first, _ := safeMediaRel(ws, "shot.png", 1)
	writeBlenderTestFile(t, ws, first)
	second, err := safeMediaRel(ws, "shot.png", 1)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatalf("expected deduped name, got same %q", first)
	}
	if !strings.HasPrefix(second, "media/shot-") {
		t.Fatalf("dedupe shape: %q", second)
	}
	// Video forces .mp4, still forces .png.
	rv, _ := safeMediaRel(ws, "clip.mov", 24)
	if !strings.HasSuffix(rv, ".mp4") {
		t.Fatalf("video ext: %q", rv)
	}
	rs, _ := safeMediaRel(ws, "still", 1)
	if !strings.HasSuffix(rs, ".png") {
		t.Fatalf("still ext: %q", rs)
	}
}

func TestExtractHeadlessPayloadTokenIsolated(t *testing.T) {
	prefix := "__PARALLAX_BLENDER_abc123__="
	// A user print that mimics the OLD static prefix must NOT be parsed.
	stdout := "Blender 5.2.1\n" +
		"__BLENDER_MCP_RESULT__={\"result\": 999, \"error\": null}\n" +
		prefix + `{"result": {"n": 1}, "stdout": "", "stderr": "", "error": null}` + "\n" +
		"Fra:1 done\n"
	payload, cleaned := extractHeadlessPayload(stdout, prefix)
	if payload == nil {
		t.Fatal("missing payload")
	}
	got, _ := json.Marshal(payload["result"])
	if string(got) != `{"n":1}` {
		t.Fatalf("wrong payload: %v", payload)
	}
	if strings.Contains(cleaned, prefix) {
		t.Fatal("prefix leaked into cleaned output")
	}
	if !strings.Contains(cleaned, "__BLENDER_MCP_RESULT__") {
		t.Fatal("spoof line should remain as plain log text")
	}
}

func TestHeadlessWrapperUsesToken(t *testing.T) {
	prefix := "__PARALLAX_BLENDER_tok__="
	w := headlessWrapper("/tmp/scene.py", "/tmp/args.json", "import bpy", "", prefix)
	if !strings.Contains(w, prefix) {
		t.Fatal("wrapper missing token prefix")
	}
	if strings.Contains(w, "__BLENDER_MCP_RESULT__") {
		t.Fatal("wrapper still uses legacy static prefix")
	}
}

func TestEffectiveTimeoutUsesEnvDefault(t *testing.T) {
	e := BlenderEnv{Timeout: 42 * time.Second}
	if got := e.effectiveTimeout(0, 5*time.Minute, 30*time.Minute); got != 42*time.Second {
		t.Fatalf("env default ignored: %v", got)
	}
	if got := e.effectiveTimeout(60, 5*time.Minute, 30*time.Minute); got != 60*time.Second {
		t.Fatalf("explicit override broken: %v", got)
	}
	if got := e.effectiveTimeout(3600, 5*time.Minute, 30*time.Minute); got != 30*time.Minute {
		t.Fatalf("hard max not enforced: %v", got)
	}
	e2 := BlenderEnv{}
	if got := e2.effectiveTimeout(0, 5*time.Minute, 30*time.Minute); got != 5*time.Minute {
		t.Fatalf("fallback default broken: %v", got)
	}
}

func TestRenderRejectsTinyResolution(t *testing.T) {
	ws := t.TempDir()
	var applied []string
	reg := NewRegistry()
	RegisterBlender(reg, BlenderEnv{Workspace: ws, BlenderBin: "blender",
		BridgeHost: "127.0.0.1", BridgePort: 19876,
		OnApplied: func(rel string) { applied = append(applied, rel) }})
	res := reg.Execute(context.Background(), "blender_render",
		`{"code":"import bpy","width":1,"height":1}`)
	if res.OK {
		t.Fatalf("expected reject for 1x1, got %v", res.Output)
	}
}

func TestRenderRejectsDangerousCodeWithoutBlender(t *testing.T) {
	ws := t.TempDir()
	reg := NewRegistry()
	RegisterBlender(reg, BlenderEnv{Workspace: ws, BlenderBin: "blender"})
	res := reg.Execute(context.Background(), "blender_render",
		`{"code":"import subprocess\nsubprocess.run(['id'])"}`)
	if res.OK {
		t.Fatal("expected validation reject before spawning blender")
	}
	if !strings.Contains(res.Error, "not allowed") {
		t.Fatalf("unexpected error: %q", res.Error)
	}
}
