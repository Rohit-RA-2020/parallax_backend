package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"parallax/internal/llm"
	"parallax/internal/tools"
)

type scriptedTurn struct {
	deltas []llm.Delta
}

type scriptedProvider struct {
	mu    sync.Mutex
	turns []scriptedTurn
}

func (s *scriptedProvider) Stream(_ context.Context, _ llm.Request) (<-chan llm.Delta, error) {
	s.mu.Lock()
	if len(s.turns) == 0 {
		s.mu.Unlock()
		ch := make(chan llm.Delta)
		close(ch)
		return ch, nil
	}
	turn := s.turns[0]
	s.turns = s.turns[1:]
	s.mu.Unlock()

	ch := make(chan llm.Delta, len(turn.deltas))
	for _, d := range turn.deltas {
		ch <- d
	}
	close(ch)
	return ch, nil
}

func TestAgentLoopToolThenAnswer(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(llm.NewFunctionTool("probe_media", "probe", json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)),
		func(_ context.Context, args json.RawMessage) tools.Result {
			var in struct {
				Path string `json:"path"`
			}
			_ = json.Unmarshal(args, &in)
			if in.Path != "talk.mp4" {
				t.Fatalf("path=%s", in.Path)
			}
			return tools.Result{OK: true, Output: map[string]any{"duration": 12.5}}
		})

	var callDelta llm.ToolCallDelta
	callDelta.Index = 0
	callDelta.ID = "call_1"
	callDelta.Type = "function"
	callDelta.Function.Name = "probe_media"
	callDelta.Function.Arguments = `{"path":"talk.mp4"}`

	p := &scriptedProvider{turns: []scriptedTurn{
		{deltas: []llm.Delta{
			{Content: "I'll inspect the file. "},
			{ToolCalls: []llm.ToolCallDelta{callDelta}, FinishReason: "tool_calls"},
		}},
		{deltas: []llm.Delta{
			{Content: "talk.mp4 is 12.5 seconds long.", FinishReason: "stop"},
		}},
	}}

	ag := &Agent{Provider: p, Tools: reg, MaxIters: 5}
	var events []Event
	out := ag.Run(context.Background(), Input{
		SessionID: "s1",
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: SystemPrompt},
			{Role: llm.RoleUser, Content: "how long is talk.mp4?"},
		},
	}, func(ev Event) { events = append(events, ev) })

	if out.Reason != "stop" {
		t.Fatalf("reason=%s", out.Reason)
	}
	if out.Iterations != 2 {
		t.Fatalf("iters=%d", out.Iterations)
	}

	var text strings.Builder
	var sawCall, sawResult bool
	for _, ev := range events {
		switch ev.Type {
		case EventText:
			var p TextPayload
			_ = json.Unmarshal(ev.Data, &p)
			text.WriteString(p.Delta)
		case EventToolCall:
			sawCall = true
		case EventToolResult:
			sawResult = true
		}
	}
	if !sawCall || !sawResult {
		t.Fatalf("missing tool events: call=%v result=%v events=%d", sawCall, sawResult, len(events))
	}
	if !strings.Contains(text.String(), "12.5 seconds") {
		t.Fatalf("text=%q", text.String())
	}

	// History must include assistant tool_calls + tool result + final answer.
	roles := make([]llm.Role, 0, len(out.Messages))
	for _, m := range out.Messages {
		roles = append(roles, m.Role)
	}
	joined := fmtRoles(roles)
	if !strings.Contains(joined, "assistant tool user") && !strings.Contains(joined, "system user assistant tool assistant") {
		t.Fatalf("history roles: %s", joined)
	}
}

func fmtRoles(r []llm.Role) string {
	parts := make([]string, len(r))
	for i, v := range r {
		parts[i] = string(v)
	}
	return strings.Join(parts, " ")
}

func TestTrimKeepsSystem(t *testing.T) {
	msgs := []llm.Message{{Role: llm.RoleSystem, Content: "sys"}}
	for i := 0; i < 20; i++ {
		msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: "u"})
		msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Content: "a"})
	}
	got := Trim(msgs, 6)
	if got[0].Role != llm.RoleSystem {
		t.Fatal("system dropped")
	}
	if len(got) > 6 {
		t.Fatalf("len=%d", len(got))
	}
}
