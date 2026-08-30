package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	. "parallax/internal/agent"
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
		func(ctx context.Context, args json.RawMessage) tools.Result {
			var in struct {
				Path string `json:"path"`
			}
			_ = json.Unmarshal(args, &in)
			if in.Path != "talk.mp4" {
				t.Fatalf("path=%s", in.Path)
			}
			tools.ReportProgress(ctx, tools.Progress{Phase: "Probing", Percent: 50})
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
	var sawCall, sawProgress, sawResult bool
	for _, ev := range events {
		switch ev.Type {
		case EventText:
			var p TextPayload
			_ = json.Unmarshal(ev.Data, &p)
			text.WriteString(p.Delta)
		case EventToolCall:
			sawCall = true
		case EventToolProgress:
			var p ToolProgressPayload
			_ = json.Unmarshal(ev.Data, &p)
			sawProgress = p.ID == "call_1" && p.Name == "probe_media" && p.Phase == "Probing" && p.Percent == 50
		case EventToolResult:
			sawResult = true
		}
	}
	if !sawCall || !sawProgress || !sawResult {
		t.Fatalf("missing tool events: call=%v progress=%v result=%v events=%d", sawCall, sawProgress, sawResult, len(events))
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

func TestAgentLoopRunsIndependentToolsInParallelAndPreservesMessageOrder(t *testing.T) {
	reg := tools.NewRegistry()
	started := make(chan string, 2)
	release := make(chan struct{})
	tool := llm.NewFunctionTool("generate_asset", "generate", json.RawMessage(`{"type":"object"}`))
	reg.RegisterParallel(tool, func(_ context.Context, raw json.RawMessage) tools.Result {
		var in struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(raw, &in)
		started <- in.Name
		<-release
		return tools.Result{OK: true, Output: map[string]any{"name": in.Name}}
	}, func(json.RawMessage) bool { return true })

	call := func(index int, id, name string) llm.ToolCallDelta {
		var delta llm.ToolCallDelta
		delta.Index = index
		delta.ID = id
		delta.Type = "function"
		delta.Function.Name = "generate_asset"
		delta.Function.Arguments = fmt.Sprintf(`{"name":%q}`, name)
		return delta
	}
	p := &scriptedProvider{turns: []scriptedTurn{
		{deltas: []llm.Delta{{ToolCalls: []llm.ToolCallDelta{
			call(0, "call-image", "image"), call(1, "call-audio", "audio"),
		}, FinishReason: "tool_calls"}}},
		{deltas: []llm.Delta{{Content: "Assets generated.", FinishReason: "stop"}}},
	}}

	done := make(chan Outcome, 1)
	go func() {
		done <- (&Agent{Provider: p, Tools: reg, MaxIters: 3, MaxParallelTools: 2}).Run(
			context.Background(), Input{SessionID: "parallel", Messages: []llm.Message{{Role: llm.RoleUser, Content: "make both"}}}, nil,
		)
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("independent tool calls did not start concurrently")
		}
	}
	close(release)
	out := <-done

	var toolIDs []string
	for _, message := range out.Messages {
		if message.Role == llm.RoleTool {
			toolIDs = append(toolIDs, message.ToolCallID)
		}
	}
	if got := strings.Join(toolIDs, ","); got != "call-image,call-audio" {
		t.Fatalf("tool message order = %s", got)
	}
}

func fmtRoles(r []llm.Role) string {
	parts := make([]string, len(r))
	for i, v := range r {
		parts[i] = string(v)
	}
	return strings.Join(parts, " ")
}

func TestAgentLoopEmitsThinking(t *testing.T) {
	p := &scriptedProvider{turns: []scriptedTurn{
		{deltas: []llm.Delta{
			{Reasoning: "I should inspect the timeline first."},
			{Content: "Looking now.", FinishReason: "stop"},
		}},
	}}
	ag := &Agent{Provider: p, Tools: tools.NewRegistry(), MaxIters: 2}
	var events []Event
	ag.Run(context.Background(), Input{
		SessionID: "s-think",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "what is on the timeline?"},
		},
	}, func(ev Event) { events = append(events, ev) })

	var thought strings.Builder
	var sawFull bool
	for _, ev := range events {
		if ev.Type != EventThinking {
			continue
		}
		var payload ThinkingPayload
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			t.Fatal(err)
		}
		thought.WriteString(payload.Delta)
		if payload.Text == "I should inspect the timeline first." {
			sawFull = true
		}
	}
	if thought.String() != "I should inspect the timeline first." {
		t.Fatalf("deltas=%q", thought.String())
	}
	if !sawFull {
		t.Fatal("missing coalesced thinking text")
	}
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

func TestTrimStartsAtUserInsteadOfOrphanedToolSequence(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleUser, Content: "old request"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID: "old-call", Type: "function", Function: llm.FunctionCall{Name: "get_timeline", Arguments: `{}`},
		}}},
		{Role: llm.RoleTool, Name: "get_timeline", ToolCallID: "old-call", Content: `{}`},
	}
	for i := 0; i < 39; i++ {
		msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf("u-%d", i)})
		msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Content: fmt.Sprintf("a-%d", i)})
	}

	got := Trim(msgs, 80)
	if len(got) > 80 {
		t.Fatalf("len=%d", len(got))
	}
	if got[0].Role != llm.RoleSystem {
		t.Fatal("system dropped")
	}
	if len(got) < 2 || got[1].Role != llm.RoleUser {
		t.Fatalf("history starts with %s after system", got[1].Role)
	}
	if got[1].Content != "u-0" {
		t.Fatalf("unexpected first retained turn %q", got[1].Content)
	}
}
