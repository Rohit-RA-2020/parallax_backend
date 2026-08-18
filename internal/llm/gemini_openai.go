package llm

import (
	"bytes"
	"encoding/json"
	"net"
	"net/url"
	"strings"
)

// GeminiOpenAIEndpoint reports whether baseURL is Google's official
// OpenAI-compatible Chat Completions host. Other providers reject
// extra_body.google, so this must stay host-gated.
func GeminiOpenAIEndpoint(baseURL string) bool {
	host := requestHost(baseURL)
	if host == "generativelanguage.googleapis.com" {
		return true
	}
	return host == "aiplatform.googleapis.com" ||
		strings.HasSuffix(host, "-aiplatform.googleapis.com") ||
		strings.HasSuffix(host, ".aiplatform.googleapis.com")
}

func requestHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		parsed, err = url.Parse("https://" + raw)
		if err != nil {
			return ""
		}
	}
	host := strings.ToLower(parsed.Host)
	if h, _, splitErr := net.SplitHostPort(host); splitErr == nil {
		return h
	}
	return host
}

type geminiExtraBody struct {
	Google geminiGoogleBody `json:"google"`
}

type geminiGoogleBody struct {
	ThinkingConfig geminiThinkingConfig `json:"thinking_config"`
}

type geminiThinkingConfig struct {
	IncludeThoughts bool   `json:"include_thoughts"`
	ThinkingLevel   string `json:"thinking_level,omitempty"`
	ThinkingBudget  *int   `json:"thinking_budget,omitempty"`
}

func (c *CompatClient) encodeStreamRequest(req Request) wireRequest {
	out := wireRequest{
		Model:       c.Model,
		Messages:    EncodeChatMessages(req.Messages),
		Tools:       req.Tools,
		ToolChoice:  req.ToolChoice,
		Stream:      true,
		Temperature: req.Temperature,
	}
	if GeminiOpenAIEndpoint(c.BaseURL) {
		out.ExtraBody = &geminiExtraBody{Google: geminiGoogleBody{ThinkingConfig: geminiThinkingFor(c.Model, req.ReasoningEffort)}}
		return out
	}
	out.ReasoningEffort = req.ReasoningEffort
	return out
}

func geminiThinkingFor(model string, effort ThinkingEffort) geminiThinkingConfig {
	cfg := geminiThinkingConfig{IncludeThoughts: true}
	if effort == "" {
		effort = DefaultThinkingEffort
	}
	if geminiUsesBudget(model) {
		budget := geminiBudgetFor(effort)
		cfg.ThinkingBudget = &budget
		return cfg
	}
	cfg.ThinkingLevel = string(effort)
	return cfg
}

func geminiUsesBudget(model string) bool {
	name := strings.ToLower(model)
	return strings.Contains(name, "2.5") || strings.Contains(name, "2.0")
}

func geminiBudgetFor(effort ThinkingEffort) int {
	switch effort {
	case ThinkingEffortLow:
		return 1024
	case ThinkingEffortHigh:
		return 24576
	default:
		return 8192
	}
}

type geminiExtraContent struct {
	Google struct {
		Thought *bool `json:"thought"`
	} `json:"google"`
}

// applyGeminiThought moves thought-flagged content out of the visible
// answer. Signatures are ignored — they are not readable traces.
func applyGeminiThought(d *Delta, extra json.RawMessage) {
	if d == nil || len(bytes.TrimSpace(extra)) == 0 {
		return
	}
	var wrap geminiExtraContent
	if json.Unmarshal(extra, &wrap) != nil || wrap.Google.Thought == nil || !*wrap.Google.Thought {
		return
	}
	if d.Content == "" {
		return
	}
	d.Reasoning += d.Content
	d.Content = ""
}
