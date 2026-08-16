package ffmpeg_test

import (
	"testing"

	. "parallax/internal/ffmpeg"
)

func TestParseMediaProbeVideo(t *testing.T) {
	got, err := ParseMediaProbe(`{
		"streams":[{"width":1080,"height":1920,"codec_type":"video"}],
		"format":{"duration":"30.533000"}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if got.Width != 1080 || got.Height != 1920 {
		t.Fatalf("frame=%dx%d", got.Width, got.Height)
	}
	if got.Duration < 30.5 || got.Duration > 30.6 {
		t.Fatalf("duration=%v", got.Duration)
	}
	if !got.HasVideo || got.HasAudio {
		t.Fatalf("streams video=%v audio=%v", got.HasVideo, got.HasAudio)
	}
}

func TestParseMediaProbeSkipsAudioStream(t *testing.T) {
	got, err := ParseMediaProbe(`{
		"streams":[
			{"codec_type":"audio"},
			{"width":1920,"height":1080,"codec_type":"video"}
		],
		"format":{"duration":"8"}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if got.Width != 1920 || got.Height != 1080 {
		t.Fatalf("frame=%dx%d", got.Width, got.Height)
	}
	if !got.HasVideo || !got.HasAudio {
		t.Fatalf("streams video=%v audio=%v", got.HasVideo, got.HasAudio)
	}
}
