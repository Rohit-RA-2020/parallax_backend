package llm

import (
	"strings"
	"unicode"
)

var thoughtOpen = []string{"<thought>", "<think>"}
var thoughtClose = []string{"</thought>", "</think>"}

// thoughtSplitter pulls Gemini-style <thought> regions out of a token stream
// so they never land in the visible assistant answer.
type thoughtSplitter struct {
	inThought bool
}

func (s *thoughtSplitter) Feed(text string) (thought, visible string) {
	if text == "" {
		return "", ""
	}
	var th, vis strings.Builder
	i := 0
	for i < len(text) {
		if !s.inThought {
			if n := tagPrefix(text[i:], thoughtOpen); n > 0 {
				s.inThought = true
				i += n
				continue
			}
			if n := tagPrefix(text[i:], thoughtClose); n > 0 {
				i += n
				continue
			}
			vis.WriteByte(text[i])
			i++
			continue
		}
		if n := tagPrefix(text[i:], thoughtClose); n > 0 {
			s.inThought = false
			i += n
			continue
		}
		if n := tagPrefix(text[i:], thoughtOpen); n > 0 {
			i += n
			continue
		}
		th.WriteByte(text[i])
		i++
	}
	return th.String(), vis.String()
}

func tagPrefix(text string, tags []string) int {
	for _, tag := range tags {
		if len(text) < len(tag) {
			continue
		}
		if strings.EqualFold(text[:len(tag)], tag) {
			n := len(tag)
			for n < len(text) && unicode.IsSpace(rune(text[n])) && text[n] != '\n' {
				n++
			}
			return n
		}
	}
	return 0
}
