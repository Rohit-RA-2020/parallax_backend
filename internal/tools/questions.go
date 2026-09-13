// Package tools - ask_questions: structured clarifying Q&A for human-in-the-loop.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"parallax/internal/llm"
)

// QuestionOption is one selectable answer for a clarifying question.
// The model provides these; the user may also type a custom response.
type QuestionOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Question is a single clarifying question from the model.
type Question struct {
	ID           string           `json:"id"`
	Question     string           `json:"question"`
	Options      []QuestionOption `json:"options"`
	AllowCustom  bool             `json:"allow_custom"`
	MultiSelect  bool             `json:"multi_select"`
}

// QuestionsEnv carries no dependencies today; kept for symmetry with other tools.
type QuestionsEnv struct{}

// RegisterQuestions exposes the ask_questions function tool.
// It is intentionally serial (barrier): when called, the agent loop pauses
// and waits for the user's answers instead of continuing.
func RegisterQuestions(reg *Registry, _ QuestionsEnv) {
	reg.Register(llm.NewFunctionTool(
		"ask_questions",
		"Ask the user 1-4 clarifying questions when requirements are ambiguous. "+
			"Call this instead of asking in plain text when you need decisions before proceeding. "+
			"Batch ALL questions in ONE call. Each question must include 2-4 selectable options plus allow_custom for free text. "+
			"Keep questions short. Do not call any other tool in the same turn.",
		json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array","minItems":1,"maxItems":4,"items":{"type":"object","properties":{"id":{"type":"string"},"question":{"type":"string"},"options":{"type":"array","minItems":2,"maxItems":4,"items":{"type":"object","properties":{"id":{"type":"string"},"label":{"type":"string"}},"required":["id","label"]}},"allow_custom":{"type":"boolean"},"multi_select":{"type":"boolean"}},"required":["id","question","options"]}}},"required":["questions"]}`),
	), askQuestions)
}

var _ = QuestionsEnv{}

func askQuestions(_ context.Context, raw json.RawMessage) Result {
	var in struct {
		Questions []struct {
			ID         string `json:"id"`
			Question   string `json:"question"`
			Options    []struct {
				ID    string `json:"id"`
				Label string `json:"label"`
			} `json:"options"`
			AllowCustom *bool `json:"allow_custom"`
			MultiSelect *bool `json:"multi_select"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{OK: false, Error: "invalid ask_questions arguments: " + err.Error()}
	}
	if len(in.Questions) < 1 || len(in.Questions) > 4 {
		return Result{OK: false, Error: "ask 1-4 questions in a single call"}
	}
	out := make([]Question, 0, len(in.Questions))
	seen := map[string]bool{}
	for i, q := range in.Questions {
		id := strings.TrimSpace(q.ID)
		if id == "" {
			id = fmt.Sprintf("q%d", i+1)
		}
		if seen[id] {
			return Result{OK: false, Error: fmt.Sprintf("duplicate question id %q", id)}
		}
		seen[id] = true
		text := strings.TrimSpace(q.Question)
		if text == "" {
			return Result{OK: false, Error: fmt.Sprintf("question %q has empty text", id)}
		}
		if len(q.Options) < 2 || len(q.Options) > 4 {
			return Result{OK: false, Error: fmt.Sprintf("question %q needs 2-4 options", id)}
		}
		opts := make([]QuestionOption, 0, len(q.Options))
		optSeen := map[string]bool{}
		for j, o := range q.Options {
			oid := strings.TrimSpace(o.ID)
			if oid == "" {
				oid = fmt.Sprintf("q%d_o%d", i+1, j+1)
			}
			if optSeen[oid] {
				return Result{OK: false, Error: fmt.Sprintf("duplicate option id %q in %q", oid, id)}
			}
			optSeen[oid] = true
			label := strings.TrimSpace(o.Label)
			if label == "" {
				return Result{OK: false, Error: fmt.Sprintf("empty option label in %q", id)}
			}
			opts = append(opts, QuestionOption{ID: oid, Label: label})
		}
		allowCustom := true
		if q.AllowCustom != nil {
			allowCustom = *q.AllowCustom
		}
		multi := false
		if q.MultiSelect != nil {
			multi = *q.MultiSelect
		}
		out = append(out, Question{ID: id, Question: text, Options: opts, AllowCustom: allowCustom, MultiSelect: multi})
	}
	return Result{OK: true, Output: map[string]any{"questions": out}}
}
