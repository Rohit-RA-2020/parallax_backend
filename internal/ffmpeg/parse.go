package ffmpeg

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Tokenize splits an ffmpeg command into argv without invoking a shell.
// Unquoted shell metacharacters are rejected so the model cannot sneak
// `rm`, pipes, or substitutions through a command string.
func Tokenize(command string) ([]string, error) {
	s := strings.TrimSpace(command)
	if s == "" {
		return nil, fmt.Errorf("empty command")
	}

	var (
		tokens []string
		buf    strings.Builder
		quote  rune
	)
	flush := func() {
		if buf.Len() == 0 {
			return
		}
		tokens = append(tokens, buf.String())
		buf.Reset()
	}

	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size

		if quote == 0 {
			if unicode.IsSpace(r) {
				flush()
				continue
			}
			if r == '\'' || r == '"' {
				quote = r
				continue
			}
			if isMeta(r) {
				return nil, fmt.Errorf("refusing shell metacharacter %q in command", string(r))
			}
			buf.WriteRune(r)
			continue
		}

		if r == quote {
			quote = 0
			continue
		}
		if quote == '"' && (r == '$' || r == '`' || r == '\\') {
			return nil, fmt.Errorf("refusing expansion character %q inside double quotes", string(r))
		}
		buf.WriteRune(r)
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote in command")
	}
	flush()
	if len(tokens) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	return tokens, nil
}

func isMeta(r rune) bool {
	switch r {
	case ';', '|', '&', '<', '>', '`', '$', '\n', '\r', '(', ')':
		return true
	}
	return false
}
