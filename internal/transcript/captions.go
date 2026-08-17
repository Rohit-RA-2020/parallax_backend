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
	lang := NormalizeCaptionLang(language)
	src := NormalizeCaptionLang(doc.Language)
	switch lang {
	case "", "original", "source", "auto", "spoken":
		return "original", nil
	case "en":
		return "en", nil
	}
	if src != "" && (lang == src || strings.HasPrefix(src, lang) || strings.HasPrefix(lang, src)) {
		return "original", nil
	}
	return lang, nil
}

// NormalizeCaptionLang maps common names to short codes used in filenames.
func NormalizeCaptionLang(language string) string {
	lang := strings.ToLower(strings.TrimSpace(language))
	lang = strings.ReplaceAll(lang, "_", "-")
	if i := strings.IndexByte(lang, '-'); i > 0 {
		lang = lang[:i]
	}
	switch lang {
	case "eng", "english":
		return "en"
	case "hin", "hindi":
		return "hi"
	case "spa", "es", "spanish", "espanol", "español":
		return "es"
	case "fra", "fre", "fr", "french", "francais", "français":
		return "fr"
	case "deu", "ger", "de", "german", "deutsch":
		return "de"
	case "por", "pt", "portuguese", "portugues", "português":
		return "pt"
	case "ita", "it", "italian":
		return "it"
	case "jpn", "ja", "jp", "japanese":
		return "ja"
	case "kor", "ko", "korean":
		return "ko"
	case "zho", "chi", "zh", "chinese", "mandarin":
		return "zh"
	case "ara", "ar", "arabic":
		return "ar"
	case "rus", "ru", "russian":
		return "ru"
	case "ben", "bn", "bangla", "bengali":
		return "bn"
	case "tam", "ta", "tamil":
		return "ta"
	case "tel", "te", "telugu":
		return "te"
	case "mar", "mr", "marathi":
		return "mr"
	case "urd", "ur", "urdu":
		return "ur"
	}
	return lang
}

// CaptionLanguageName is a short UI label for a language code.
func CaptionLanguageName(code string) string {
	switch NormalizeCaptionLang(code) {
	case "en":
		return "English"
	case "hi":
		return "Hindi"
	case "es":
		return "Spanish"
	case "fr":
		return "French"
	case "de":
		return "German"
	case "pt":
		return "Portuguese"
	case "it":
		return "Italian"
	case "ja":
		return "Japanese"
	case "ko":
		return "Korean"
	case "zh":
		return "Chinese"
	case "ar":
		return "Arabic"
	case "ru":
		return "Russian"
	case "bn":
		return "Bengali"
	case "ta":
		return "Tamil"
	case "te":
		return "Telugu"
	case "mr":
		return "Marathi"
	case "ur":
		return "Urdu"
	case "original", "":
		return "Original"
	default:
		return strings.ToUpper(NormalizeCaptionLang(code))
	}
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

// ParseSRT reads SubRip cues. Invalid blocks are skipped.
func ParseSRT(body string) []Cue {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	blocks := strings.Split(body, "\n\n")
	var cues []Cue
	for _, block := range blocks {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		lines := strings.Split(block, "\n")
		if len(lines) < 2 {
			continue
		}
		idx := 0
		if _, err := fmt.Sscanf(strings.TrimSpace(lines[0]), "%d", new(int)); err == nil && strings.Contains(lines[1], "-->") {
			idx = 1
		}
		if idx >= len(lines) || !strings.Contains(lines[idx], "-->") {
			continue
		}
		start, end, ok := parseSRTRange(lines[idx])
		if !ok || end < start {
			continue
		}
		text := strings.TrimSpace(strings.Join(lines[idx+1:], "\n"))
		if text == "" {
			continue
		}
		cues = append(cues, Cue{Start: start, End: end, Text: text})
	}
	return cues
}

func parseSRTRange(line string) (float64, float64, bool) {
	parts := strings.Split(line, "-->")
	if len(parts) != 2 {
		return 0, 0, false
	}
	start, ok1 := parseSRTStamp(parts[0])
	end, ok2 := parseSRTStamp(parts[1])
	return start, end, ok1 && ok2
}

func parseSRTStamp(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		s = s[:i]
	}
	s = strings.ReplaceAll(s, ",", ".")
	var h, m int
	var sec float64
	if _, err := fmt.Sscanf(s, "%d:%d:%f", &h, &m, &sec); err != nil {
		return 0, false
	}
	if h < 0 || m < 0 || m > 59 || sec < 0 {
		return 0, false
	}
	return float64(h)*3600 + float64(m)*60 + sec, true
}

// ShiftCues adds delta seconds to every cue. Delta may be negative.
func ShiftCues(cues []Cue, delta float64) []Cue {
	if delta == 0 || len(cues) == 0 {
		return cues
	}
	out := make([]Cue, 0, len(cues))
	for _, cue := range cues {
		cue.Start += delta
		cue.End += delta
		if cue.End <= 0 {
			continue
		}
		if cue.Start < 0 {
			cue.Start = 0
		}
		out = append(out, cue)
	}
	return out
}

// ClipCues keeps cues that overlap [start, start+duration).
func ClipCues(cues []Cue, start, duration float64) []Cue {
	end := start + duration
	if duration <= 0 {
		return nil
	}
	var out []Cue
	for _, cue := range cues {
		if cue.End <= start || cue.Start >= end {
			continue
		}
		if cue.Start < start {
			cue.Start = start
		}
		if cue.End > end {
			cue.End = end
		}
		if cue.End > cue.Start {
			out = append(out, cue)
		}
	}
	return out
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
