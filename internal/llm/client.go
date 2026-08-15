package llm

import "context"

// ChatProvider is the only interface the agent depends on.
// Implementations talk to a concrete HTTP API; the agent never does.
type ChatProvider interface {
	// Stream yields incremental deltas and closes the channel when the
	// completion (or an error) is finished. A terminal error is delivered
	// as Delta.Err; the channel is then closed.
	Stream(ctx context.Context, req Request) (<-chan Delta, error)
}

// CompletionsURL joins a configured base URL with /chat/completions.
// Accepts "https://host/v1", "https://host/v1/", or a full completions URL.
func CompletionsURL(base string) string {
	if base == "" {
		return ""
	}
	for len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	const suffix = "/chat/completions"
	if len(base) >= len(suffix) && base[len(base)-len(suffix):] == suffix {
		return base
	}
	return base + suffix
}
