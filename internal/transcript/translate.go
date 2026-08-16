package transcript

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"parallax/internal/llm"
)

const translateBatch = 20

// TranslateSegments fills TextEN using the chat LLM. English source is copied through.
func TranslateSegments(ctx context.Context, completer llm.Completer, language string, segments []Segment) error {
	if completer == nil {
		return fmt.Errorf("translator is not configured")
	}
	need := make([]int, 0, len(segments))
	for i := range segments {
		orig := strings.TrimSpace(segments[i].Text)
		if orig == "" {
			continue
		}
		if looksEnglish(language) && isMostlyLatin(orig) {
			segments[i].TextEN = orig
			continue
		}
		need = append(need, i)
	}
	for start := 0; start < len(need); start += translateBatch {
		end := start + translateBatch
		if end > len(need) {
			end = len(need)
		}
		batchIdx := need[start:end]
		inputs := make([]string, len(batchIdx))
		for i, idx := range batchIdx {
			inputs[i] = segments[idx].Text
		}
		out, err := translateBatchJSON(ctx, completer, language, inputs)
		if err != nil {
			return err
		}
		if len(out) != len(inputs) {
			return fmt.Errorf("translator returned %d strings for %d segments", len(out), len(inputs))
		}
		for i, idx := range batchIdx {
			segments[idx].TextEN = strings.TrimSpace(out[i])
		}
	}
	return nil
}

func translateBatchJSON(ctx context.Context, completer llm.Completer, language string, inputs []string) ([]string, error) {
	var b strings.Builder
	b.WriteString("Translate each numbered line into natural English. Keep the same number of items, same order, and the same meaning. Do not add commentary.\n")
	if lang := strings.TrimSpace(language); lang != "" && !looksEnglish(lang) {
		fmt.Fprintf(&b, "Source language code: %s.\n", lang)
	}
	b.WriteString("Return ONLY a JSON array of strings.\n\n")
	for i, line := range inputs {
		fmt.Fprintf(&b, "%d. %s\n", i+1, line)
	}
	temp := llm.Ptr(0.0)
	raw, err := completer.Complete(ctx, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "You translate speech transcript segments into English. Output a JSON array of strings and nothing else."},
			{Role: llm.RoleUser, Content: b.String()},
		},
		Temperature: temp,
	})
	if err != nil {
		return nil, err
	}
	return parseStringArray(raw)
}

func parseStringArray(raw string) ([]string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "["); i >= 0 {
		if j := strings.LastIndex(s, "]"); j > i {
			s = s[i : j+1]
		}
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("translator JSON: %w", err)
	}
	return out, nil
}
