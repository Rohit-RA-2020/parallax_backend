package agent

import "encoding/json"

// EventType is the SSE event name streamed to clients.
type EventType string

const (
	EventSession        EventType = "session"
	EventStep           EventType = "step"
	EventText           EventType = "text"
	EventThinking       EventType = "thinking"
	EventToolCall       EventType = "tool_call"
	EventToolResult     EventType = "tool_result"
	EventDone           EventType = "done"
	EventError          EventType = "error"
	EventProjectChanged EventType = "project_changed"
)

// Event is one realtime update from the agent loop.
type Event struct {
	Type EventType       `json:"type"`
	Data json.RawMessage `json:"data"`
}

func NewEvent(t EventType, payload any) Event {
	b, err := json.Marshal(payload)
	if err != nil {
		b = []byte(`{}`)
	}
	return Event{Type: t, Data: b}
}

type Sink func(Event)

type SessionPayload struct {
	SessionID string `json:"session_id"`
}

type StepPayload struct {
	Iteration int    `json:"iteration"`
	Phase     string `json:"phase"`
}

type TextPayload struct {
	Delta string `json:"delta"`
}

type ThinkingPayload struct {
	Delta     string `json:"delta,omitempty"`
	Text      string `json:"text,omitempty"`
	Iteration int    `json:"iteration"`
}

type ToolCallPayload struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Iteration int             `json:"iteration"`
}

type ToolResultPayload struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	OK        bool   `json:"ok"`
	Output    any    `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms"`
	Iteration int    `json:"iteration"`
}

type DonePayload struct {
	Reason     string `json:"reason"`
	Iterations int    `json:"iterations"`
	SessionID  string `json:"session_id"`
}

type ErrorPayload struct {
	Message string `json:"message"`
}

type ProjectChangedPayload struct {
	ProjectID       string `json:"project_id"`
	Revision        int    `json:"revision"`
	TimelineChanged bool   `json:"timeline_changed"`
}
