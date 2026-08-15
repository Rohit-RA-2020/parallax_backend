// Package llm is a provider-agnostic Chat Completions client.
// Any OpenAI-compatible endpoint (xAI, OpenAI, Groq, Together, OpenRouter,
// Ollama, …) can be used by changing base URL, API key, and model.
package llm

import "encoding/json"

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is the OpenAI-compatible chat message used both on the wire and
// inside the agent loop.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// ToolCall is a completed function invocation requested by the model.
type ToolCall struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Function     FunctionCall    `json:"function"`
	ExtraContent json.RawMessage `json:"extra_content,omitempty"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolSpec is the Chat Completions "tools" entry.
// Nested `function` form is the most widely implemented dialect.
type ToolSpec struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

func NewFunctionTool(name, description string, parameters json.RawMessage) ToolSpec {
	return ToolSpec{
		Type: "function",
		Function: ToolFunction{
			Name:        name,
			Description: description,
			Parameters:  parameters,
		},
	}
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Delta is one incremental piece of a streamed completion.
type Delta struct {
	Content      string
	ToolCalls    []ToolCallDelta
	FinishReason string
	Usage        *Usage
	Err          error
}

// ToolCallDelta is the streaming form of a tool call. Providers differ:
// OpenAI streams name/arguments across many chunks; xAI may emit the whole
// call in a single chunk. The agent accumulates either shape.
type ToolCallDelta struct {
	Index        int             `json:"index"`
	ID           string          `json:"id,omitempty"`
	Type         string          `json:"type,omitempty"`
	ExtraContent json.RawMessage `json:"extra_content,omitempty"`
	Function     struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

// Request is the provider-neutral chat request.
type Request struct {
	Messages    []Message
	Tools       []ToolSpec
	ToolChoice  any
	Temperature *float64
}

func Ptr[T any](v T) *T { return &v }
