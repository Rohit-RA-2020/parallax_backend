package transcript_test

import (
	"context"
	"strings"
	"testing"

	"parallax/internal/llm"
	. "parallax/internal/transcript"
)

type scriptedCompleter struct {
	reply string
	err   error
	seen  *string
}

func (s scriptedCompleter) Complete(_ context.Context, req llm.Request) (string, error) {
	if s.seen != nil && len(req.Messages) > 0 {
		*s.seen = req.Messages[len(req.Messages)-1].Content
	}
	return s.reply, s.err
}

func TestTranslateSegmentsSkipsEnglish(t *testing.T) {
	segs := []Segment{{ID: "seg-0000", Text: "Thanks for coming"}}
	if err := TranslateSegments(context.Background(), scriptedCompleter{reply: `["nope"]`}, "en", segs); err != nil {
		t.Fatal(err)
	}
	if segs[0].TextEN != "Thanks for coming" {
		t.Fatalf("text_en=%q", segs[0].TextEN)
	}
}

func TestTranslateSegmentsUsesJSONArray(t *testing.T) {
	var seen string
	segs := []Segment{{ID: "a", Text: "धन्यवाद"}, {ID: "b", Text: "आओ"}}
	err := TranslateSegments(context.Background(), scriptedCompleter{
		reply: "```json\n[\"Thanks\",\"Come in\"]\n```",
		seen:  &seen,
	}, "hi", segs)
	if err != nil {
		t.Fatal(err)
	}
	if segs[0].TextEN != "Thanks" || segs[1].TextEN != "Come in" {
		t.Fatalf("segs=%+v", segs)
	}
	if !strings.Contains(seen, "1. धन्यवाद") {
		t.Fatalf("prompt=%q", seen)
	}
}
