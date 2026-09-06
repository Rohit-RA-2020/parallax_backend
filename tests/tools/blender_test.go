package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	. "parallax/internal/tools"
)

func lookPath(bin string) (string, error) { return exec.LookPath(bin) }

func blenderTestEnv(ws string, applied *[]string) BlenderEnv {
	return BlenderEnv{
		Workspace: ws, BlenderBin: "blender",
		BridgeHost: "127.0.0.1", BridgePort: 19876, // nothing listens here
		OnMutation: func() {},
		OnApplied:  func(rel string) { *applied = append(*applied, rel) },
	}
}

func TestBlenderSceneInfoBridgeDown(t *testing.T) {
	ws := t.TempDir()
	var applied []string
	reg := NewRegistry()
	RegisterBlender(reg, blenderTestEnv(ws, &applied))
	res := reg.Execute(context.Background(), "blender_scene_info", `{}`)
	if res.OK {
		t.Fatal("expected failure when bridge is down")
	}
}

func TestBlenderRunScriptHeadlessFallback(t *testing.T) {
	if _, err := lookPath("blender"); err != nil {
		t.Skip("blender not on PATH")
	}
	ws := t.TempDir()
	var applied []string
	reg := NewRegistry()
	RegisterBlender(reg, blenderTestEnv(ws, &applied))
	res := reg.Execute(context.Background(), "blender_run_script",
		`{"code":"import bpy\n__result__ = {'objects': len(bpy.data.objects)}"}`)
	if !res.OK {
		t.Fatalf("run_script: %s", res.Error)
	}
	out := res.Output.(map[string]any)
	if out["transport"] != "headless" {
		t.Fatalf("transport=%v out=%v", out["transport"], out)
	}
}

func TestBlenderRenderStill(t *testing.T) {
	if _, err := lookPath("blender"); err != nil {
		t.Skip("blender not on PATH")
	}
	ws := t.TempDir()
	var applied []string
	reg := NewRegistry()
	RegisterBlender(reg, blenderTestEnv(ws, &applied))
	code := `import bpy
bpy.ops.mesh.primitive_cube_add(location=(0,0,1))
mat = bpy.data.materials.new('TestRed')
mat.use_nodes = True
mat.node_tree.nodes['Principled BSDF'].inputs['Base Color'].default_value = (1, 0.1, 0.1, 1)
bpy.context.active_object.data.materials.append(mat)
bpy.ops.object.light_add(type='SUN', location=(5,-5,10))`
	args, _ := json.Marshal(map[string]any{
		"code": code, "filename": "test-cube.png", "width": 320, "height": 180,
	})
	res := reg.Execute(context.Background(), "blender_render", string(args))
	if !res.OK {
		t.Fatalf("render: %s", res.Error)
	}
	out := res.Output.(map[string]any)
	rel, _ := out["path"].(string)
	if rel != "media/test-cube.png" {
		t.Fatalf("path=%v out=%v", rel, out)
	}
	st, err := os.Stat(filepath.Join(ws, rel))
	if err != nil || st.Size() == 0 {
		t.Fatalf("render output missing: %v size=%v", rel, st)
	}
	if len(applied) != 1 || applied[0] != rel {
		t.Fatalf("OnApplied=%v", applied)
	}
}
