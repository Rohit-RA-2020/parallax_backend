// Package tools is the agent's function registry.
// The model only sees JSON schemas; execution stays in-process.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"parallax/internal/llm"
)

// Result is what the model sees after a tool runs.
type Result struct {
	OK      bool          `json:"ok"`
	Name    string        `json:"name"`
	Output  any           `json:"output,omitempty"`
	Error   string        `json:"error,omitempty"`
	Elapsed time.Duration `json:"-"`
}

func (r Result) JSON() string {
	b, err := json.Marshal(r)
	if err != nil {
		return `{"ok":false,"error":"failed to encode tool result"}`
	}
	return string(b)
}

// Handler executes a tool with already-parsed JSON arguments.
type Handler func(ctx context.Context, args json.RawMessage) Result

// ParallelPolicy reports whether one invocation is independent of other tool
// calls in the same model turn. Tools are serial by default; handlers opt in
// only when their arguments do not mutate shared ordered state.
type ParallelPolicy func(args json.RawMessage) bool

// Progress is an optional realtime update emitted by long-running tools.
type Progress struct {
	Phase   string  `json:"phase,omitempty"`
	Current int64   `json:"current,omitempty"`
	Total   int64   `json:"total,omitempty"`
	Percent float64 `json:"percent,omitempty"`
}

type progressSink func(Progress)
type progressContextKey struct{}

// WithProgress attaches a progress sink to a tool execution context.
func WithProgress(ctx context.Context, sink func(Progress)) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, progressContextKey{}, progressSink(sink))
}

// ReportProgress sends an update when the caller supplied a progress sink.
func ReportProgress(ctx context.Context, progress Progress) {
	if sink, ok := ctx.Value(progressContextKey{}).(progressSink); ok && sink != nil {
		sink(progress)
	}
}

type spec struct {
	tool           llm.ToolSpec
	handler        Handler
	parallelPolicy ParallelPolicy
}

// Registry maps tool names to schemas + handlers.
type Registry struct {
	mu    sync.RWMutex
	items map[string]spec
	order []string
}

func NewRegistry() *Registry {
	return &Registry{items: map[string]spec{}}
}

func (r *Registry) Register(tool llm.ToolSpec, h Handler) {
	r.register(tool, h, nil)
}

// RegisterParallel registers a tool that may run concurrently when policy
// approves the specific invocation. A nil policy is treated as serial.
func (r *Registry) RegisterParallel(tool llm.ToolSpec, h Handler, policy ParallelPolicy) {
	r.register(tool, h, policy)
}

func (r *Registry) register(tool llm.ToolSpec, h Handler, policy ParallelPolicy) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := tool.Function.Name
	if _, exists := r.items[name]; !exists {
		r.order = append(r.order, name)
	}
	r.items[name] = spec{tool: tool, handler: h, parallelPolicy: policy}
}

func (r *Registry) Specs() []llm.ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]llm.ToolSpec, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.items[name].tool)
	}
	return out
}

func (r *Registry) Execute(ctx context.Context, name, arguments string) Result {
	r.mu.RLock()
	item, ok := r.items[name]
	r.mu.RUnlock()
	if !ok {
		return Result{OK: false, Name: name, Error: fmt.Sprintf("unknown tool %q", name)}
	}

	raw := json.RawMessage(arguments)
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if !json.Valid(raw) {
		return Result{OK: false, Name: name, Error: "tool arguments are not valid JSON"}
	}

	start := time.Now()
	res := item.handler(ctx, raw)
	res.Name = name
	res.Elapsed = time.Since(start)
	return res
}

// ParallelSafe reports whether the registered tool permits this invocation to
// run alongside adjacent independent calls from the same assistant message.
func (r *Registry) ParallelSafe(name, arguments string) bool {
	r.mu.RLock()
	item, ok := r.items[name]
	r.mu.RUnlock()
	if !ok || item.parallelPolicy == nil {
		return false
	}
	raw := json.RawMessage(arguments)
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if !json.Valid(raw) {
		return false
	}
	return item.parallelPolicy(raw)
}
