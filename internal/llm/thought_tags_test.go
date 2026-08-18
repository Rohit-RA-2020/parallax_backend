package llm

import "testing"

func TestThoughtSplitterExtractsTaggedRegions(t *testing.T) {
	var s thoughtSplitter
	thought, visible := s.Feed("<thought>**Plan**\nInspect first.\n</thought>I can recut the scene.")
	if thought != "**Plan**\nInspect first.\n" {
		t.Fatalf("thought=%q", thought)
	}
	if visible != "I can recut the scene." {
		t.Fatalf("visible=%q", visible)
	}
}

func TestThoughtSplitterStreamsAcrossChunks(t *testing.T) {
	var s thoughtSplitter
	t1, v1 := s.Feed("<thought>Defining")
	t2, v2 := s.Feed(" capabilities.\n</thought>")
	t3, v3 := s.Feed("I am Director.")
	if t1+t2+t3 != "Defining capabilities.\n" {
		t.Fatalf("thought=%q", t1+t2+t3)
	}
	if v1+v2+v3 != "I am Director." {
		t.Fatalf("visible=%q", v1+v2+v3)
	}
}

func TestThoughtSplitterDropsStrayCloseTag(t *testing.T) {
	var s thoughtSplitter
	thought, visible := s.Feed("</thought>Hello")
	if thought != "" {
		t.Fatalf("thought=%q", thought)
	}
	if visible != "Hello" {
		t.Fatalf("visible=%q", visible)
	}
}
