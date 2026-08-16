package transcript_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	. "parallax/internal/transcript"
)

func TestFasterWhisperParsesScriptJSON(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "transcribe.py")
	wav := filepath.Join(dir, "talk.wav")
	if err := os.WriteFile(wav, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := "#!/usr/bin/env python3\nimport json,sys\nprint(json.dumps({'ok':True,'language':'hi','model':'large-v3-turbo','segments':[{'id':'seg-0000','start':0.1,'end':1.2,'text':'नमस्ते'}],'words':[{'start':0.1,'end':0.6,'text':'नमस्ते'}]}))\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := (&FasterWhisper{Python: "python3", Script: script, Model: "large-v3-turbo"}).Transcribe(context.Background(), wav, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Language != "hi" || len(got.Segments) != 1 || got.Segments[0].Text != "नमस्ते" {
		t.Fatalf("got=%+v", got)
	}
}
