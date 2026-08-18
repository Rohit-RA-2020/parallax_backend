package tools

import (
	"strings"
	"testing"

	"parallax/internal/elevenlabs"
)

func TestTranscriptFromAlignmentBuildsTimedWordsAndSegments(t *testing.T) {
	doc := transcriptFromAlignment("Hello world!", "en", &elevenlabs.Alignment{
		Characters:          []string{"H", "e", "l", "l", "o", " ", "w", "o", "r", "l", "d", "!"},
		CharacterStartTimes: []float64{0, .05, .1, .15, .2, .25, .3, .35, .4, .45, .5, .55},
		CharacterEndTimes:   []float64{.05, .1, .15, .2, .25, .3, .35, .4, .45, .5, .55, .6},
	})
	if len(doc.Words) != 2 || doc.Words[0].Text != "Hello" || doc.Words[1].Text != "world!" {
		t.Fatalf("words=%+v", doc.Words)
	}
	if len(doc.Segments) != 1 || doc.Segments[0].Start != 0 || doc.Segments[0].End != .6 {
		t.Fatalf("segments=%+v", doc.Segments)
	}
}

func TestTranscriptFromAlignmentLeavesNonEnglishTextForTranslation(t *testing.T) {
	doc := transcriptFromAlignment("नमस्ते", "hi", &elevenlabs.Alignment{
		Characters:          []string{"नमस्ते"},
		CharacterStartTimes: []float64{0},
		CharacterEndTimes:   []float64{0.4},
	})
	if len(doc.Segments) != 1 || doc.Segments[0].TextEN != "" {
		t.Fatalf("expected untranslated segment, got=%+v", doc.Segments)
	}
}

func TestCompositionLyricsSupportsMusicV1AndV2Plans(t *testing.T) {
	if got := compositionLyrics([]byte(`{"chunks":[{"text":"[Verse]\nhello"}]}`)); got == "" {
		t.Fatal("expected v2 lyrics")
	}
	if got := compositionLyrics([]byte(`{"sections":[{"lines":["hello","world"]}]}`)); got != "hello\nworld" {
		t.Fatalf("lyrics=%q", got)
	}
}

func TestBuildGeminiMusicPromptAddsLyriaGuidance(t *testing.T) {
	prompt, err := buildGeminiMusicPrompt("cinematic strings in D minor", "lyria-3-pro-preview", float64Ptr(90), true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "Target duration: 90 seconds") || !strings.Contains(prompt, "Instrumental only, no vocals") {
		t.Fatalf("prompt=%q", prompt)
	}
	if _, err := buildGeminiMusicPrompt("short clip", "lyria-3-clip-preview", float64Ptr(20), false); err == nil {
		t.Fatal("expected clip duration validation")
	}
}

func float64Ptr(value float64) *float64 { return &value }
