package transcript

import (
	"context"
	"strings"
	"unicode"
)

// ProgressFunc reports decode position in seconds.
type ProgressFunc func(at, duration float64)

// SegmentFunc is called as soon as Whisper finishes one phrase.
type SegmentFunc func(seg Segment, words []Word)

// Transcriber turns a 16 kHz mono wav into words and segments.
type Transcriber interface {
	Transcribe(ctx context.Context, wavPath string, progress ProgressFunc) (ASRResult, error)
}

// StreamTranscriber emits segments while decoding so they can be embedded live.
type StreamTranscriber interface {
	TranscribeStream(ctx context.Context, wavPath string, progress ProgressFunc, onSeg SegmentFunc) (ASRResult, error)
}

// ASRResult is one Whisper pass over a file.
type ASRResult struct {
	Language string
	Model    string
	Device   string
	Words    []Word
	Segments []Segment
}

func lastLines(s string, n int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(no output)"
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

func looksEnglish(lang string) bool {
	lang = strings.ToLower(strings.TrimSpace(lang))
	switch lang {
	case "", "en", "eng", "english":
		return true
	default:
		return false
	}
}

func isMostlyLatin(s string) bool {
	letters := 0
	latin := 0
	for _, r := range s {
		if !unicode.IsLetter(r) {
			continue
		}
		letters++
		if r <= unicode.MaxASCII {
			latin++
		}
	}
	if letters == 0 {
		return true
	}
	return latin*100/letters >= 90
}
