package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// CompatClient talks to any OpenAI-compatible /v1/chat/completions endpoint.
type CompatClient struct {
	BaseURL    string
	APIKey     string
	Model      string
	HTTPClient *http.Client
	// ExtraHeaders are copied onto every request (e.g. x-grok-conv-id).
	ExtraHeaders map[string]string
}

func NewCompatClient(baseURL, apiKey, model string) *CompatClient {
	return &CompatClient{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Model:   model,
		HTTPClient: &http.Client{
			// Streamed completions can last many minutes on reasoning models.
			Timeout: 0,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          16,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 120 * time.Second,
			},
		},
	}
}

type wireRequest struct {
	Model           string         `json:"model"`
	Messages        []wireMessage  `json:"messages"`
	Tools           []ToolSpec     `json:"tools,omitempty"`
	ToolChoice      any            `json:"tool_choice,omitempty"`
	Stream          bool           `json:"stream"`
	Temperature     *float64       `json:"temperature,omitempty"`
	ReasoningEffort ThinkingEffort `json:"reasoning_effort,omitempty"`
}

type wireMessage struct {
	Role       Role            `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
}

type wireError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
	Message string `json:"message"`
}

type wireChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string          `json:"role"`
			Content   *string         `json:"content"`
			ToolCalls []ToolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (c *CompatClient) Stream(ctx context.Context, req Request) (<-chan Delta, error) {
	if strings.TrimSpace(c.Model) == "" {
		return nil, fmt.Errorf("llm: model is not configured")
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return nil, fmt.Errorf("llm: api key is not configured")
	}
	url := CompletionsURL(c.BaseURL)
	if url == "" {
		return nil, fmt.Errorf("llm: base_url is not configured")
	}

	body, err := json.Marshal(wireRequest{
		Model:           c.Model,
		Messages:        EncodeChatMessages(req.Messages),
		Tools:           req.Tools,
		ToolChoice:      req.ToolChoice,
		Stream:          true,
		Temperature:     req.Temperature,
		ReasoningEffort: req.ReasoningEffort,
	})
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	for k, v := range c.ExtraHeaders {
		if v != "" {
			httpReq.Header.Set(k, v)
		}
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("llm request: %w", err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, readAPIError(resp)
	}

	out := make(chan Delta, 16)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		if err := parseSSE(resp.Body, out); err != nil {
			select {
			case <-ctx.Done():
			case out <- Delta{Err: err}:
			}
		}
	}()
	return out, nil
}

type wireCompletion struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete performs a non-streaming chat completion and returns assistant text.
func (c *CompatClient) Complete(ctx context.Context, req Request) (string, error) {
	if strings.TrimSpace(c.Model) == "" {
		return "", fmt.Errorf("llm: model is not configured")
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return "", fmt.Errorf("llm: api key is not configured")
	}
	url := CompletionsURL(c.BaseURL)
	if url == "" {
		return "", fmt.Errorf("llm: base_url is not configured")
	}

	body, err := json.Marshal(wireRequest{
		Model:           c.Model,
		Messages:        EncodeChatMessages(req.Messages),
		Temperature:     req.Temperature,
		ReasoningEffort: req.ReasoningEffort,
		Stream:          false,
	})
	if err != nil {
		return "", err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range c.ExtraHeaders {
		if v != "" {
			httpReq.Header.Set(k, v)
		}
	}

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("llm request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", readAPIError(resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", fmt.Errorf("llm: read completion: %w", err)
	}
	var parsed wireCompletion
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("llm: decode completion: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", fmt.Errorf("llm: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("llm: empty completion")
	}
	return parsed.Choices[0].Message.Content, nil
}

// sanitizeMessages repairs streamed OpenAI-compatible argument glitches:
// a complete object may be emitted more than once (`{}{}`) or two objects may
// be concatenated (`{"path":"a.mp4"}{}`). Stored sessions can retain that
// value, so normalize again immediately before sending a request.
func sanitizeMessages(messages []Message) []Message {
	out := make([]Message, len(messages))
	copy(out, messages)
	for i := range out {
		if len(out[i].ToolCalls) == 0 {
			continue
		}
		out[i].ToolCalls = append([]ToolCall(nil), out[i].ToolCalls...)
		for j := range out[i].ToolCalls {
			out[i].ToolCalls[j].Function.Arguments = normalizeToolArguments(out[i].ToolCalls[j].Function.Arguments)
		}
	}
	return out
}

func normalizeToolArguments(arguments string) string {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return `{}`
	}
	if json.Valid([]byte(trimmed)) {
		return trimmed
	}

	decoder := json.NewDecoder(strings.NewReader(trimmed))
	var values []json.RawMessage
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if err == io.EOF {
				break
			}
			return arguments
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			return arguments
		}
		values = append(values, append(json.RawMessage(nil), compact.Bytes()...))
	}
	if len(values) == 0 {
		return arguments
	}
	if len(values) == 1 {
		return string(values[0])
	}

	allSame := true
	for i := 1; i < len(values); i++ {
		if !bytes.Equal(values[0], values[i]) {
			allSame = false
			break
		}
	}
	if allSame {
		return string(values[0])
	}

	// Gemini/OpenAI-compat streams sometimes emit two complete objects for one
	// call, e.g. {"path":"media/talk.mp4"}{}. Keep one valid object so the
	// registry can execute instead of failing json.Valid.
	if merged, ok := mergeJSONObjects(values); ok {
		return merged
	}
	return string(values[len(values)-1])
}

func mergeJSONObjects(values []json.RawMessage) (string, bool) {
	merged := map[string]any{}
	for _, raw := range values {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return "", false
		}
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			return "", false
		}
		for key, value := range obj {
			merged[key] = value
		}
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return "", false
	}
	return string(out), true
}

func completeJSON(s string) bool {
	s = strings.TrimSpace(s)
	return s != "" && json.Valid([]byte(s))
}

func compactJSON(s string) ([]byte, bool) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(strings.TrimSpace(s))); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
}

func readAPIError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var we wireError
	if json.Unmarshal(raw, &we) == nil {
		msg := we.Error.Message
		if msg == "" {
			msg = we.Message
		}
		if msg != "" {
			return fmt.Errorf("llm: %s (http %d)", msg, resp.StatusCode)
		}
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Errorf("llm: provider returned http %d", resp.StatusCode)
	}
	return fmt.Errorf("llm: provider returned http %d: %s", resp.StatusCode, truncate(string(raw), 400))
}

// parseSSE reads an OpenAI-style event stream and forwards decoded deltas.
func parseSSE(r io.Reader, out chan<- Delta) error {
	sc := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1<<20)

	var data lines
	flush := func() error {
		payload := data.Join()
		data = data[:0]
		if payload == "" || payload == "[DONE]" {
			return nil
		}
		var chunk wireChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return fmt.Errorf("llm: decode stream chunk: %w", err)
		}
		if chunk.Error != nil && chunk.Error.Message != "" {
			return fmt.Errorf("llm: %s", chunk.Error.Message)
		}
		d := Delta{Usage: chunk.Usage}
		if len(chunk.Choices) > 0 {
			ch := chunk.Choices[0]
			if ch.Delta.Content != nil {
				d.Content = *ch.Delta.Content
			}
			d.ToolCalls = ch.Delta.ToolCalls
			if ch.FinishReason != nil {
				d.FinishReason = *ch.FinishReason
			}
		}
		if d.Content == "" && len(d.ToolCalls) == 0 && d.FinishReason == "" && d.Usage == nil {
			return nil
		}
		out <- d
		return nil
	}

	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimSpace(rest))
			continue
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return flush()
}

type lines []string

func (l lines) Join() string {
	if len(l) == 0 {
		return ""
	}
	if len(l) == 1 {
		return l[0]
	}
	return strings.Join(l, "\n")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

type toolCallAcc struct {
	id, typ, name, args string
	extra               json.RawMessage
}

func (a *toolCallAcc) applyMeta(d ToolCallDelta) {
	if d.ID != "" {
		a.id = d.ID
	}
	if d.Type != "" {
		a.typ = d.Type
	}
	if len(d.ExtraContent) > 0 {
		a.extra = append(a.extra[:0], d.ExtraContent...)
	}
}

// AssembleToolCalls folds streaming tool-call deltas into complete ToolCalls.
// Handles incremental OpenAI-style chunks, single-shot xAI-style chunks, and
// Gemini OpenAI-compat collisions where parallel calls reuse index 0.
func AssembleToolCalls(deltas []ToolCallDelta) []ToolCall {
	byIdx := map[int]*toolCallAcc{}
	var accs []*toolCallAcc
	start := func(index int) *toolCallAcc {
		a := &toolCallAcc{typ: "function"}
		byIdx[index] = a
		accs = append(accs, a)
		return a
	}

	for _, d := range deltas {
		a, ok := byIdx[d.Index]
		if !ok {
			a = start(d.Index)
		} else if toolCallBoundary(a.id, a.name, d) {
			a = start(d.Index)
		} else if d.Function.Arguments != "" && completeJSON(a.args) && completeJSON(d.Function.Arguments) {
			// Same call emitted two complete objects. Dedup or merge; do not
			// concatenate into {"path":"..."}{} which json.Valid rejects.
			if left, okLeft := compactJSON(a.args); okLeft {
				if right, okRight := compactJSON(d.Function.Arguments); okRight && bytes.Equal(left, right) {
					a.applyMeta(d)
					continue
				}
			}
			a.args = normalizeToolArguments(a.args + d.Function.Arguments)
			a.applyMeta(d)
			if d.Function.Name != "" {
				a.name = d.Function.Name
			}
			continue
		}

		a.applyMeta(d)
		if d.Function.Name != "" {
			a.name = d.Function.Name
		}
		if d.Function.Arguments != "" {
			a.args += d.Function.Arguments
		}
	}

	out := make([]ToolCall, 0, len(accs))
	for _, a := range accs {
		if a.name == "" {
			continue
		}
		out = append(out, ToolCall{
			ID:           a.id,
			Type:         a.typ,
			ExtraContent: a.extra,
			Function: FunctionCall{
				Name:      a.name,
				Arguments: normalizeToolArguments(a.args),
			},
		})
	}
	return out
}

func toolCallBoundary(id, name string, d ToolCallDelta) bool {
	if d.ID != "" && id != "" && d.ID != id {
		return true
	}
	return d.Function.Name != "" && name != "" && d.Function.Name != name
}
