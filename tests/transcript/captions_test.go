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
}
