package httpapi

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"parallax/internal/config"
	"parallax/internal/llm"
)

const chatTitlePrompt = `Create a concise title for a chat based on the user's first message. Return only the title, with no quotes, markdown, or ending punctuation. Use the user's language. Keep it specific and under 60 characters.`

func (s *Server) generateChatTitle(parent context.Context, cfg config.LLM, firstMessage string) <-chan string {
	result := make(chan string, 1)
	go func() {
		defer close(result)
		provider := s.NewLLM(cfg)
		completer, ok := provider.(llm.Completer)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(parent, 30*time.Second)
		defer cancel()
		title, err := completer.Complete(ctx, llm.Request{
			Messages: []llm.Message{
				{Role: llm.RoleSystem, Content: chatTitlePrompt},
				{Role: llm.RoleUser, Content: strings.TrimSpace(firstMessage)},
			},
			Temperature:     llm.Ptr(0.2),
			ReasoningEffort: llm.ThinkingEffortLow,
		})
		if err != nil {
			s.log().Warn("generate chat title", "err", err)
			return
		}
		result <- cleanGeneratedChatTitle(title)
	}()
	return result
}

func isFirstChatMessage(messages []llm.Message) bool {
	for _, message := range messages {
		if message.Role == llm.RoleUser {
			return false
		}
	}
	return true
}

func isDefaultGeneratedTitle(title string) bool {
	switch strings.ToLower(strings.TrimSpace(title)) {
	case "", "new chat", "untitled", "director":
		return true
	default:
		return false
	}
}

func cleanGeneratedChatTitle(title string) string {
	title = strings.TrimSpace(title)
	title = strings.Trim(title, "\"'`*_# ")
	title = strings.TrimSpace(strings.TrimRight(title, ".!?;:"))
	if i := strings.IndexAny(title, "\r\n"); i >= 0 {
		title = strings.TrimSpace(title[:i])
	}
	if utf8.RuneCountInString(title) > 80 {
		runes := []rune(title)
		title = strings.TrimSpace(string(runes[:80]))
	}
	return title
}
