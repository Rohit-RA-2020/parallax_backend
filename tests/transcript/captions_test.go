package transcript_test

import (
	"strings"
	"testing"

	. "parallax/internal/transcript"
)

func TestWriteSRTAndCaptionLanguage(t *testing.T) {
	doc := &Document{
		Language: "hi",
		Segments: []Segment{
			{Start: 4.12, End: 6.08, Text: "धन्यवाद आने के लिए", TextEN: "Thanks for coming in"},
			{Start: 6.2, End: 8, Text: "आओ", TextEN: "Come in"},
		},
	}
	orig, mode, err := CaptionCues(doc, "original")
	if err != nil || mode != "original" || orig[0].Text != "धन्यवाद आने के लिए" {
		t.Fatalf("orig=%+v mode=%s err=%v", orig, mode, err)
	}
	en, mode, err := CaptionCues(doc, "en")
	if err != nil || mode != "en" || en[0].Text != "Thanks for coming in" {
		t.Fatalf("en=%+v mode=%s err=%v", en, mode, err)
	}
	if _, mode, err = CaptionCues(doc, "hi"); err != nil || mode != "original" {
		t.Fatalf("hi mode=%s err=%v", mode, err)
	}
	es, mode, err := CaptionCues(doc, "es")
	if err != nil || mode != "es" || es[0].Text != "धन्यवाद आने के लिए" {
		t.Fatalf("es=%+v mode=%s err=%v", es, mode, err)
	}
	srt := WriteSRT(orig)
	if !strings.Contains(srt, "00:00:04,120 --> 00:00:06,080") || !strings.Contains(srt, "धन्यवाद आने के लिए") {
		t.Fatalf("srt=%s", srt)
	}
	parsed := ParseSRT(srt)
	if len(parsed) != 2 || parsed[0].Start != 4.12 || parsed[1].Text != "आओ" {
		t.Fatalf("parsed=%+v", parsed)
	}
	shifted := ShiftCues(parsed, 10)
	if shifted[0].Start < 14.11 || shifted[0].Start > 14.13 {
		t.Fatalf("shifted=%+v", shifted)
	}
	clipped := ClipCues(shifted, 10, 5)
	if len(clipped) != 1 {
		t.Fatalf("clipped=%+v", clipped)
	}
	if NormalizeCaptionLang("hindi") != "hi" || CaptionLanguageName("hi") != "Hindi" {
		t.Fatal("language alias")
	}
	if CaptionLangISO6392("hi") != "hin" {
		t.Fatal("iso639-2")
	}
	vtt := WriteVTT(orig)
	if !strings.HasPrefix(vtt, "WEBVTT") || !strings.Contains(vtt, "00:00:04.120 --> 00:00:06.080") {
		t.Fatalf("vtt=%s", vtt)
	}
}
