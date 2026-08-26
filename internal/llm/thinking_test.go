package llm

import "testing"

func TestNormalizeThinkingEffortAcceptsNone(t *testing.T) {
	got, err := NormalizeThinkingEffort(" none ")
	if err != nil {
		t.Fatal(err)
	}
	if got != ThinkingEffortNone {
		t.Fatalf("effort=%q", got)
	}
}

func TestEncodeStreamRequestKeepsNoneForCompatibleProviders(t *testing.T) {
	client := NewCompatClient("https://api.openai.com/v1", "key", "gpt-5.6")
	got := client.encodeStreamRequest(Request{ReasoningEffort: ThinkingEffortNone})
	if got.ReasoningEffort != ThinkingEffortNone {
		t.Fatalf("reasoning_effort=%q", got.ReasoningEffort)
	}
}
