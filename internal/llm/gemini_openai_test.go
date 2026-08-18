package llm

import "testing"

func TestGeminiOpenAIEndpoint(t *testing.T) {
	yes := []string{
		"https://generativelanguage.googleapis.com/v1beta/openai",
		"https://generativelanguage.googleapis.com/v1beta/openai/",
		"https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/us-central1/endpoints/openapi",
		"https://aiplatform.googleapis.com/v1beta1/projects/p/locations/global/endpoints/openapi",
	}
	for _, raw := range yes {
		if !GeminiOpenAIEndpoint(raw) {
			t.Errorf("expected gemini host %s", raw)
		}
	}
	no := []string{
		"https://api.x.ai/v1",
		"https://api.openai.com/v1",
		"https://openrouter.ai/api/v1",
		"http://127.0.0.1:11434/v1",
	}
	for _, raw := range no {
		if GeminiOpenAIEndpoint(raw) {
			t.Errorf("unexpected gemini host %s", raw)
		}
	}
}

func TestEncodeStreamRequestGatesGeminiExtraBody(t *testing.T) {
	gemini := NewCompatClient("https://generativelanguage.googleapis.com/v1beta/openai", "key", "gemini-3.7-flash")
	got := gemini.encodeStreamRequest(Request{
		Messages:        []Message{{Role: RoleUser, Content: "hi"}},
		ReasoningEffort: ThinkingEffortLow,
	})
	if got.ReasoningEffort != "" {
		t.Fatalf("gemini should omit reasoning_effort, got %q", got.ReasoningEffort)
	}
	if got.ExtraBody == nil || !got.ExtraBody.Google.ThinkingConfig.IncludeThoughts {
		t.Fatalf("missing include_thoughts: %#v", got.ExtraBody)
	}
	if got.ExtraBody.Google.ThinkingConfig.ThinkingLevel != "low" {
		t.Fatalf("thinking_level=%q", got.ExtraBody.Google.ThinkingConfig.ThinkingLevel)
	}

	flash := NewCompatClient("https://generativelanguage.googleapis.com/v1beta/openai", "key", "gemini-2.5-flash")
	budgeted := flash.encodeStreamRequest(Request{ReasoningEffort: ThinkingEffortHigh})
	if budgeted.ExtraBody == nil || budgeted.ExtraBody.Google.ThinkingConfig.ThinkingBudget == nil {
		t.Fatalf("2.5 should set thinking_budget: %#v", budgeted.ExtraBody)
	}
	if *budgeted.ExtraBody.Google.ThinkingConfig.ThinkingBudget != 24576 {
		t.Fatalf("budget=%v", *budgeted.ExtraBody.Google.ThinkingConfig.ThinkingBudget)
	}

	xai := NewCompatClient("https://api.x.ai/v1", "key", "grok-4.6")
	plain := xai.encodeStreamRequest(Request{ReasoningEffort: ThinkingEffortMedium})
	if plain.ExtraBody != nil {
		t.Fatalf("xAI must not send extra_body: %#v", plain.ExtraBody)
	}
	if plain.ReasoningEffort != ThinkingEffortMedium {
		t.Fatalf("reasoning_effort=%q", plain.ReasoningEffort)
	}
}
