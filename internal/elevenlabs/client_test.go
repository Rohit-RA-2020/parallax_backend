package elevenlabs

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGenerateSpeechWithTimestamps(t *testing.T) {
	audio := []byte("speech-audio")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/text-to-speech/voice%2Fone/with-timestamps" && r.URL.Path != "/v1/text-to-speech/voice/one/with-timestamps" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if r.Header.Get("xi-api-key") != "secret" {
			t.Fatalf("missing api key")
		}
		if r.URL.Query().Get("output_format") != "mp3_44100_192" {
			t.Fatalf("query=%s", r.URL.RawQuery)
		}
		var body struct {
			Text    string `json:"text"`
			ModelID string `json:"model_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Text != "Hello world" || body.ModelID != "eleven_v3" {
			t.Fatalf("body=%+v", body)
		}
		_ = json.NewEncoder(w).Encode(SpeechResponse{
			AudioBase64: base64.StdEncoding.EncodeToString(audio),
			Alignment:   &Alignment{Characters: []string{"H", "i"}, CharacterStartTimes: []float64{0, 0.1}, CharacterEndTimes: []float64{0.1, 0.2}},
		})
	}))
	defer server.Close()

	client := NewClient("secret", server.URL, time.Minute, 1<<20)
	got, err := client.GenerateSpeechWithTimestamps(context.Background(), SpeechRequest{VoiceID: "voice/one", Text: "Hello world", ModelID: "eleven_v3"}, "mp3_44100_192")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeAudioBase64(got.AudioBase64, 1<<20)
	if err != nil || !bytes.Equal(decoded, audio) {
		t.Fatalf("audio=%q err=%v", decoded, err)
	}
}

func TestComposeMusicStreamsAndCapturesSongID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/music" || r.URL.Query().Get("output_format") != "auto" {
			t.Fatalf("url=%s", r.URL.String())
		}
		var body MusicRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Prompt != "soft piano" || body.ModelID != "music_v2" {
			t.Fatalf("body=%+v", body)
		}
		w.Header().Set("song-id", "song-123")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("music-bytes"))
	}))
	defer server.Close()
	client := NewClient("secret", server.URL, time.Minute, 1<<20)
	var out bytes.Buffer
	got, err := client.ComposeMusic(context.Background(), MusicRequest{Prompt: "soft piano", ModelID: "music_v2"}, "auto", &out)
	if err != nil {
		t.Fatal(err)
	}
	if got.SongID != "song-123" || got.Bytes != int64(out.Len()) || out.String() != "music-bytes" {
		t.Fatalf("result=%+v output=%q", got, out.String())
	}
}

func TestSoundEffectValidationAndProviderError(t *testing.T) {
	client := NewClient("secret", "http://127.0.0.1:1", time.Second, 1<<20)
	var out bytes.Buffer
	if _, err := client.GenerateSoundEffect(context.Background(), SoundEffectRequest{Text: "boom", DurationSeconds: float64Ptr(0.1)}, "", &out); err == nil {
		t.Fatal("expected duration validation")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"detail":"invalid prompt"}`))
	}))
	defer server.Close()
	client = NewClient("secret", server.URL, time.Minute, 1<<20)
	if _, err := client.GenerateSoundEffect(context.Background(), SoundEffectRequest{Text: "boom"}, "", &out); err == nil || !strings.Contains(err.Error(), "422") {
		t.Fatalf("error=%v", err)
	}
}

func float64Ptr(value float64) *float64 { return &value }
