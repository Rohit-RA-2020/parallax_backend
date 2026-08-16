package transcript

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"parallax/internal/llm"
)

// Cue is one timed caption line.
type Cue struct {
	Start float64
	End   float64
	Text  string
}

// CaptionCues builds timed lines in the requested language.
// language is original/source/auto (spoken language), en, or another target code.
func CaptionCues(doc *Document, language string) ([]Cue, string, error) {
	if doc == nil || len(doc.Segments) == 0 {
		return nil, "", fmt.Errorf("no transcript segments")
	}
	mode, err := captionMode(doc, language)
	if err != nil {
		return nil, "", err
	}
	cues := make([]Cue, 0, len(doc.Segments))
	for _, seg := range doc.Segments {
		text := strings.TrimSpace(seg.Text)
		if mode == "en" {
			text = strings.TrimSpace(seg.TextEN)
			if text == "" {
				text = strings.TrimSpace(seg.Text)
			}
		}
		if text == "" {
			continue
		}
		end := seg.End
		if end <= seg.Start {
			end = seg.Start + 0.8
		}
		cues = append(cues, Cue{Start: seg.Start, End: end, Text: wrapCaption(text)})
	}
	if len(cues) == 0 {
		return nil, "", fmt.Errorf("transcript has no captionable text")
	}
	return cues, mode, nil
}

func captionMode(doc *Document, language string) (string, error) {
	lang := strings.ToLower(strings.TrimSpace(language))
	src := strings.ToLower(strings.TrimSpace(doc.Language))
	switch lang {
	case "", "original", "source", "auto", "spoken":
		return "original", nil
	case "en", "eng", "english":
		return "en", nil
	}
	if src != "" && (lang == src || strings.HasPrefix(src, lang) || strings.HasPrefix(lang, src)) {
		return "original", nil
	}
	return lang, nil
}

// TranslateCues rewrites cue text into targetLang, keeping timings.
func TranslateCues(ctx context.Context, completer llm.Completer, cues []Cue, targetLang string) error {
	if completer == nil {
		return fmt.Errorf("cannot translate captions: no chat model configured")
	}
	if looksEnglish(targetLang) {
		return nil
	}
	texts := make([]string, len(cues))
	for i, cue := range cues {
		texts[i] = cue.Text
	}
	for start := 0; start < len(texts); start += translateBatch {
		end := start + translateBatch
		if end > len(texts) {
			end = len(texts)
		}
		out, err := translateCaptionBatch(ctx, completer, targetLang, texts[start:end])
		if err != nil {
			return err
		}
		if len(out) != end-start {
			return fmt.Errorf("caption translator returned %d lines for %d cues", len(out), end-start)
		}
		for i, line := range out {
			cues[start+i].Text = wrapCaption(strings.TrimSpace(line))
		}
	}
	return nil
}

func translateCaptionBatch(ctx context.Context, completer llm.Completer, target string, inputs []string) ([]string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Translate each numbered caption into %s. Keep the same number of items and the same meaning. Keep each line short enough to read on screen. Return ONLY a JSON array of strings.\n\n", target)
	for i, line := range inputs {
		fmt.Fprintf(&b, "%d. %s\n", i+1, line)
	}
	raw, err := completer.Complete(ctx, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "You translate video captions. Output a JSON array of strings and nothing else."},
			{Role: llm.RoleUser, Content: b.String()},
		},
		Temperature: llm.Ptr(0.0),
	})
	if err != nil {
		return nil, err
	}
	return parseStringArray(raw)
}

// WriteSRT renders cues as SubRip text.
func WriteSRT(cues []Cue) string {
	var b strings.Builder
	for i, cue := range cues {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n", i+1, srtTime(cue.Start), srtTime(cue.End), cue.Text)
	}
	return b.String()
}

func srtTime(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	ms := int(sec*1000 + 0.5)
	h := ms / 3600000
	ms %= 3600000
	m := ms / 60000
	ms %= 60000
	s := ms / 1000
	ms %= 1000
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}

func wrapCaption(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSpace(text)
	if text == "" {
		return text
	}
	const max = 42
	words := strings.Fields(text)
	if len(words) == 0 {
		return text
	}
	var lines []string
	var cur strings.Builder
	for _, word := range words {
		if cur.Len() == 0 {
			cur.WriteString(word)
			continue
		}
		if utf8.RuneCountInString(cur.String())+1+utf8.RuneCountInString(word) > max {
			lines = append(lines, cur.String())
			cur.Reset()
			cur.WriteString(word)
			continue
		}
		cur.WriteByte(' ')
		cur.WriteString(word)
	}
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
	}
	if len(lines) > 2 {
		lines = []string{strings.Join(lines[:len(lines)-1], " "), lines[len(lines)-1]}
		if utf8.RuneCountInString(lines[0]) > max*2 {
			lines[0] = string([]rune(lines[0])[:max*2])
		}
	}
	return strings.Join(lines, "\n")
}
