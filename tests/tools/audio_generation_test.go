package tools_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"parallax/internal/elevenlabs"
	"parallax/internal/ffmpeg"
	"parallax/internal/gemini"
	"parallax/internal/projects"
	"parallax/internal/tools"
)

func TestGenerateVoiceoverPlacesAudioAndCommitsMedia(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	store, err := projects.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.Create("Audio")
	if err != nil {
		t.Fatal(err)
	}
	wav := filepath.Join(t.TempDir(), "voice.wav")
	cmd := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "sine=f=440:d=0.2", wav)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, data)
	}
	audio, err := os.ReadFile(wav)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/text-to-speech/voice-1/with-timestamps" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(elevenlabs.SpeechResponse{
			AudioBase64: base64.StdEncoding.EncodeToString(audio),
			Alignment: &elevenlabs.Alignment{
				Characters:          []string{"H", "i"},
				CharacterStartTimes: []float64{0, 0.05},
				CharacterEndTimes:   []float64{0.05, 0.1},
			},
		})
	}))
	defer server.Close()

	voiceFile := filepath.Join(t.TempDir(), "voices.json")
	if err := os.WriteFile(voiceFile, []byte(`[{"id":"voice-1","name":"Narrator","languages":["en"],"characteristics":["warm"]}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	voices, err := elevenlabs.LoadVoiceCatalog(voiceFile)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.BeginTimelineTransaction(project.ID, projects.CommitMeta{Actor: "agent", Summary: "Generate voiceover"})
	if err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry()
	tools.RegisterAudioGeneration(reg, tools.AudioGenerationEnv{
		Workspace: project.Dir,
		Bins:      ffmpeg.Bins{FFmpeg: "ffmpeg", FFprobe: "ffprobe"},
		Client:    elevenlabs.NewClient("secret", server.URL, time.Minute, 16<<20),
		Voices:    voices,
		ProjectID: project.ID, Transaction: tx,
		Limiter:    tools.NewLimiter(1),
		OnMutation: tx.MarkMediaMutation,
	})
	result := reg.Execute(context.Background(), "generate_voiceover", `{"text":"Hi","voice_id":"voice-1","placement":{"start_frame":12,"track":"A2"}}`)
	if !result.OK {
		t.Fatalf("generate voiceover: %s output=%v", result.Error, result.Output)
	}
	out := result.Output.(map[string]any)
	path := out["path"].(string)
	if _, err := os.Stat(filepath.Join(project.Dir, filepath.FromSlash(path))); err != nil {
		t.Fatal(err)
	}
	doc, changed, err := tx.Commit()
	if err != nil || !changed {
		t.Fatalf("commit changed=%v err=%v", changed, err)
	}
	if len(doc.Clips) != 1 || doc.Clips[0].Track != "A2" || doc.Clips[0].StartFrame != 12 || doc.Clips[0].MediaPath != path {
		t.Fatalf("timeline=%+v", doc.Clips)
	}
}

func TestGenerateMusicUsesGeminiLyriaAndStoresLyrics(t *testing.T) {
	store, err := projects.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.Create("Gemini Music")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/interactions" || r.Header.Get("x-goog-api-key") != "secret" {
			t.Fatalf("request=%s key=%s", r.URL.Path, r.Header.Get("x-goog-api-key"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "lyria-3-clip-preview" || !strings.Contains(body["input"].(string), "Instrumental only, no vocals") {
			t.Fatalf("body=%+v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "gemini-interaction-1",
			"output_text": "[Verse]\nA new horizon",
			"output_audio": map[string]any{
				"data":      base64.StdEncoding.EncodeToString([]byte("music")),
				"mime_type": "audio/mpeg",
			},
		})
	}))
	defer server.Close()

	tx, err := store.BeginTimelineTransaction(project.ID, projects.CommitMeta{Actor: "agent", Summary: "Generate music"})
	if err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry()
	tools.RegisterAudioGeneration(reg, tools.AudioGenerationEnv{
		Workspace: project.Dir, Bins: ffmpeg.Bins{FFmpeg: "ffmpeg", FFprobe: "ffprobe"},
		MusicClient:      gemini.NewClient("secret", server.URL, time.Minute, 1<<20),
		GeminiMusicModel: "lyria-3-pro-preview", GeminiMusicOutputFormat: "mp3",
		ProjectID: project.ID, Transaction: tx, Limiter: tools.NewLimiter(1), OnMutation: tx.MarkMediaMutation,
	})
	result := reg.Execute(context.Background(), "generate_music", `{"prompt":"bright cinematic score","model_id":"lyria-3-clip-preview","force_instrumental":true}`)
	if !result.OK {
		t.Fatalf("generate music: %s", result.Error)
	}
	out := result.Output.(map[string]any)
	path := out["path"].(string)
	if _, err := os.Stat(filepath.Join(project.Dir, filepath.FromSlash(path))); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := tx.Commit(); err != nil || !changed {
		t.Fatalf("commit changed=%v err=%v", changed, err)
	}
}
