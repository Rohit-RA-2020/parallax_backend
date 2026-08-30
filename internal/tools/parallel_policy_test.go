package tools

import (
	"encoding/json"
	"testing"

	"parallax/internal/llm"
)

func TestRegistryToolsAreSerialUnlessExplicitlyOptedIn(t *testing.T) {
	reg := NewRegistry()
	tool := llm.NewFunctionTool("serial", "serial", json.RawMessage(`{"type":"object"}`))
	reg.Register(tool, nil)
	if reg.ParallelSafe("serial", `{}`) {
		t.Fatal("ordinary registered tool unexpectedly marked parallel-safe")
	}
	if reg.ParallelSafe("missing", `{}`) {
		t.Fatal("missing tool unexpectedly marked parallel-safe")
	}
}

func TestAssetGenerationParallelPolicies(t *testing.T) {
	image := ImageEnv{}
	if !image.parallelImageGeneration(json.RawMessage(`{"prompt":"new still"}`)) {
		t.Fatal("new still should be parallel-safe")
	}
	if image.parallelImageGeneration(json.RawMessage(`{"prompt":"edit","source":"media/a.jpg"}`)) {
		t.Fatal("in-place still edit should remain serial")
	}
	if !image.parallelImageGeneration(json.RawMessage(`{"prompt":"variant","source":"media/a.jpg","apply_to":"none"}`)) {
		t.Fatal("separate still variant should be parallel-safe")
	}

	video := VideoGenerationEnv{}
	if !video.parallelVideoGeneration(json.RawMessage(`{"prompt":"new video"}`)) {
		t.Fatal("new video should be parallel-safe")
	}
	if video.parallelVideoGeneration(json.RawMessage(`{"prompt":"edit","task":"edit","source":"media/a.mp4"}`)) {
		t.Fatal("in-place video edit should remain serial")
	}
	if !video.parallelVideoGeneration(json.RawMessage(`{"prompt":"variant","task":"edit","source":"media/a.mp4","apply_to":"none"}`)) {
		t.Fatal("separate video variant should be parallel-safe")
	}

	if !parallelWithoutPlacement(json.RawMessage(`{"prompt":"music"}`)) {
		t.Fatal("asset-only audio generation should be parallel-safe")
	}
	if parallelWithoutPlacement(json.RawMessage(`{"prompt":"music","placement":{"at":"end"}}`)) {
		t.Fatal("timeline audio placement should remain serial")
	}
}
