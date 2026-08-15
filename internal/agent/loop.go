package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"parallax/internal/llm"
	"parallax/internal/tools"
)

const defaultMaxIters = 12

// Agent is a framework-free observe → think → act loop.
// It streams text as the model produces it, executes tool calls locally,
// appends observations, and repeats until the model stops or the budget is hit.
type Agent struct {
	Provider llm.ChatProvider
	Tools    *tools.Registry
	MaxIters int
	Logger   *slog.Logger
}

type Input struct {
	SessionID string
	Messages  []llm.Message
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

// Run drives the loop and reports every step through emit.
func (a *Agent) Run(ctx context.Context, in Input, emit Sink) Outcome {
	if emit == nil {
		emit = func(Event) {}
	}
	if a.Provider == nil {
		emit(NewEvent(EventError, ErrorPayload{Message: "no LLM provider configured"}))
		return Outcome{SessionID: in.SessionID, Messages: in.Messages, Reason: "error"}
	}

	messages := append([]llm.Message(nil), in.Messages...)
	if len(messages) == 0 || messages[0].Role != llm.RoleSystem {
		messages = append([]llm.Message{{Role: llm.RoleSystem, Content: systemPrompt}}, messages...)
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
			Messages:    Trim(messages, 80),
			Tools:       specs,
			ToolChoice:  "auto",
			Temperature: temp,
		})
		if err != nil {
			emit(NewEvent(EventError, ErrorPayload{Message: err.Error()}))
			return Outcome{SessionID: in.SessionID, Messages: messages, Iterations: i, Reason: "error"}
		}

		var (
			text      strings.Builder
			toolParts []llm.ToolCallDelta
			reason    string
		)
		for d := range deltas {
			if d.Err != nil {
				emit(NewEvent(EventError, ErrorPayload{Message: d.Err.Error()}))
				return Outcome{SessionID: in.SessionID, Messages: messages, Iterations: i, Reason: "error"}
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
		for _, call := range calls {
			args := json.RawMessage(call.Function.Arguments)
			if !json.Valid(args) {
				args = json.RawMessage(`{}`)
			}
			emit(NewEvent(EventToolCall, ToolCallPayload{
				ID:        call.ID,
				Name:      call.Function.Name,
				Arguments: args,
				Iteration: i,
			}))

			res := a.Tools.Execute(ctx, call.Function.Name, call.Function.Arguments)
			emit(NewEvent(EventToolResult, ToolResultPayload{
				ID:        call.ID,
				Name:      call.Function.Name,
				OK:        res.OK,
				Output:    res.Output,
				Error:     res.Error,
				ElapsedMS: res.Elapsed.Milliseconds(),
				Iteration: i,
			}))

			messages = append(messages, llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: call.ID,
				Name:       call.Function.Name,
				Content:    clip(res.JSON(), 16<<10),
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

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + `…"}`
}
