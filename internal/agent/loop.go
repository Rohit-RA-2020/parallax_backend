package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"parallax/internal/llm"
	"parallax/internal/tools"
)

const defaultMaxIters = 12
const defaultMaxParallelTools = 4

// Agent is a framework-free observe → think → act loop.
// It streams text as the model produces it, executes tool calls locally,
// appends observations, and repeats until the model stops or the budget is hit.
type Agent struct {
	Provider llm.ChatProvider
	Tools    *tools.Registry
	MaxIters int
	// MaxParallelTools bounds independent tool calls from one model turn.
	// Tools remain serial unless their registry policy explicitly opts in.
	MaxParallelTools int
	Logger           *slog.Logger
}

type Input struct {
	SessionID      string
	Messages       []llm.Message
	ThinkingEffort llm.ThinkingEffort
}

type Outcome struct {
	SessionID  string
	Messages   []llm.Message
	Iterations int
	Reason     string
}

func (a *Agent) maxIters() int {
	if a.MaxIters < 1 {
		return defaultMaxIters
	}
	return a.MaxIters
}

func (a *Agent) log() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}

func (a *Agent) maxParallelTools() int {
	if a.MaxParallelTools < 1 {
		return defaultMaxParallelTools
	}
	return a.MaxParallelTools
}

// Run drives the loop and reports every step through emit.
func (a *Agent) Run(ctx context.Context, in Input, emit Sink) Outcome {
	if emit == nil {
		emit = func(Event) {}
	}
	// Parallel tools report progress from different goroutines. Serialize sink
	// access so SSE writers and event collectors never receive concurrent calls.
	rawEmit := emit
	var emitMu sync.Mutex
	emit = func(ev Event) {
		emitMu.Lock()
		defer emitMu.Unlock()
		rawEmit(ev)
	}
	if a.Provider == nil {
		emit(NewEvent(EventError, ErrorPayload{Message: "no LLM provider configured"}))
		return Outcome{SessionID: in.SessionID, Messages: in.Messages, Reason: "error"}
	}

	messages := append([]llm.Message(nil), in.Messages...)
	if len(messages) == 0 || messages[0].Role != llm.RoleSystem {
		messages = append([]llm.Message{{Role: llm.RoleSystem, Content: SystemPrompt}}, messages...)
	}

	specs := a.Tools.Specs()
	temp := llm.Ptr(0.2)
	max := a.maxIters()

	for i := 1; i <= max; i++ {
		if err := ctx.Err(); err != nil {
			emit(NewEvent(EventError, ErrorPayload{Message: err.Error()}))
			return Outcome{SessionID: in.SessionID, Messages: messages, Iterations: i - 1, Reason: "canceled"}
		}

		emit(NewEvent(EventStep, StepPayload{Iteration: i, Phase: "think"}))
		a.log().Info("agent step", "session", in.SessionID, "iteration", i)

		deltas, err := a.Provider.Stream(ctx, llm.Request{
			Messages:        Trim(messages, 80),
			Tools:           specs,
			ToolChoice:      "auto",
			Temperature:     temp,
			ReasoningEffort: in.ThinkingEffort,
		})
		if err != nil {
			emit(NewEvent(EventError, ErrorPayload{Message: err.Error()}))
			return Outcome{SessionID: in.SessionID, Messages: messages, Iterations: i, Reason: "error"}
		}

		var (
			text      strings.Builder
			thought   strings.Builder
			toolParts []llm.ToolCallDelta
			reason    string
		)
		for d := range deltas {
			if d.Err != nil {
				emit(NewEvent(EventError, ErrorPayload{Message: d.Err.Error()}))
				return Outcome{SessionID: in.SessionID, Messages: messages, Iterations: i, Reason: "error"}
			}
			if d.Reasoning != "" {
				thought.WriteString(d.Reasoning)
				emit(NewEvent(EventThinking, ThinkingPayload{Delta: d.Reasoning, Iteration: i}))
			}
			if d.Content != "" {
				text.WriteString(d.Content)
				emit(NewEvent(EventText, TextPayload{Delta: d.Content}))
			}
			if len(d.ToolCalls) > 0 {
				toolParts = append(toolParts, d.ToolCalls...)
			}
			if d.FinishReason != "" {
				reason = d.FinishReason
			}
		}
		if thought.Len() > 0 {
			emit(NewEvent(EventThinking, ThinkingPayload{Text: clipText(thought.String(), 16<<10), Iteration: i}))
		}

		calls := llm.AssembleToolCalls(toolParts)
		asst := llm.Message{
			Role:    llm.RoleAssistant,
			Content: text.String(),
		}
		if len(calls) > 0 {
			asst.ToolCalls = calls
		}
		messages = append(messages, asst)

		if len(calls) == 0 {
			if reason == "" {
				reason = "stop"
			}
			emit(NewEvent(EventDone, DonePayload{
				Reason:     reason,
				Iterations: i,
				SessionID:  in.SessionID,
			}))
			return Outcome{SessionID: in.SessionID, Messages: messages, Iterations: i, Reason: reason}
		}

		emit(NewEvent(EventStep, StepPayload{Iteration: i, Phase: "act"}))
		results := a.executeToolCalls(ctx, calls, i, emit)
		for index, call := range calls {
			messages = append(messages, llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: call.ID,
				Name:       call.Function.Name,
				Content:    clip(results[index].JSON(), 16<<10),
			})
		}
	}

	msg := fmt.Sprintf("stopped after %d iterations without a final answer", max)
	emit(NewEvent(EventError, ErrorPayload{Message: msg}))
	emit(NewEvent(EventDone, DonePayload{
		Reason:     "max_iterations",
		Iterations: max,
		SessionID:  in.SessionID,
	}))
	return Outcome{SessionID: in.SessionID, Messages: messages, Iterations: max, Reason: "max_iterations"}
}

// executeToolCalls runs consecutive parallel-safe calls as a bounded batch.
// A serial call is a barrier, preserving the model's requested ordering around
// timeline mutations, in-place edits, and other stateful operations.
func (a *Agent) executeToolCalls(ctx context.Context, calls []llm.ToolCall, iteration int, emit Sink) []tools.Result {
	results := make([]tools.Result, len(calls))
	for start := 0; start < len(calls); {
		if !a.Tools.ParallelSafe(calls[start].Function.Name, calls[start].Function.Arguments) {
			results[start] = a.executeToolCall(ctx, calls[start], iteration, emit)
			start++
			continue
		}
		end := start + 1
		for end < len(calls) && a.Tools.ParallelSafe(calls[end].Function.Name, calls[end].Function.Arguments) {
			end++
		}
		a.executeParallelBatch(ctx, calls[start:end], results[start:end], iteration, emit)
		start = end
	}
	return results
}

func (a *Agent) executeParallelBatch(ctx context.Context, calls []llm.ToolCall, results []tools.Result, iteration int, emit Sink) {
	limit := a.maxParallelTools()
	if limit > len(calls) {
		limit = len(calls)
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for index, call := range calls {
		index, call := index, call
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[index] = tools.Result{OK: false, Name: call.Function.Name, Error: ctx.Err().Error()}
				return
			}
			results[index] = a.executeToolCall(ctx, call, iteration, emit)
		}()
	}
	wg.Wait()
}

func (a *Agent) executeToolCall(ctx context.Context, call llm.ToolCall, iteration int, emit Sink) tools.Result {
	args := json.RawMessage(call.Function.Arguments)
	if !json.Valid(args) {
		args = json.RawMessage(`{}`)
	}
	emit(NewEvent(EventToolCall, ToolCallPayload{
		ID: call.ID, Name: call.Function.Name, Arguments: args, Iteration: iteration,
	}))
	toolCtx := tools.WithProgress(ctx, func(progress tools.Progress) {
		emit(NewEvent(EventToolProgress, ToolProgressPayload{
			ID: call.ID, Name: call.Function.Name, Phase: progress.Phase,
			Current: progress.Current, Total: progress.Total, Percent: progress.Percent,
			Iteration: iteration,
		}))
	})
	res := a.Tools.Execute(toolCtx, call.Function.Name, call.Function.Arguments)
	emit(NewEvent(EventToolResult, ToolResultPayload{
		ID: call.ID, Name: call.Function.Name, OK: res.OK, Output: res.Output, Error: res.Error,
		ElapsedMS: res.Elapsed.Milliseconds(), Iteration: iteration,
	}))
	return res
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + `…"}`
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
