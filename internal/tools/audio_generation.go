package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"parallax/internal/elevenlabs"
	"parallax/internal/ffmpeg"
	"parallax/internal/gemini"
	"parallax/internal/llm"
	"parallax/internal/projects"
	"parallax/internal/transcript"
)

type Limiter struct{ slots chan struct{} }

var generatedPathMu sync.Mutex

func NewLimiter(n int) *Limiter {
	if n < 1 {
		n = 4
	}
	return &Limiter{slots: make(chan struct{}, n)}
}

func (l *Limiter) acquire(ctx context.Context) error {
	if l == nil {
		return nil
	}
	select {
	case l.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *Limiter) release() {
	if l != nil {
		<-l.slots
	}
}

type AudioGenerationEnv struct {
	Workspace               string
	Bins                    ffmpeg.Bins
	Client                  *elevenlabs.Client
	Voices                  *elevenlabs.VoiceCatalog
	MusicClient             *gemini.Client
	TTSModel                string
	SFXModel                string
	TTSOutputFormat         string
	SFXOutputFormat         string
	GeminiMusicModel        string
	GeminiMusicOutputFormat string
	Limiter                 *Limiter
	ProjectID               string
	Transaction             *projects.TimelineTransaction
	Indexer                 *transcript.Indexer
	Logger                  *slog.Logger
	OnMutation              func()
}

type audioPlacement struct {
	At         string `json:"at,omitempty"`
	StartFrame *int   `json:"start_frame,omitempty"`
	Track      string `json:"track,omitempty"`
}

func RegisterAudioGeneration(reg *Registry, env AudioGenerationEnv) {
	reg.Register(llm.NewFunctionTool(
		"list_tts_voices",
		"List the configured ElevenLabs voices and their characteristics. Call this before generating speech so you can choose a voice that matches the requested language, tone, age, accent, and delivery.",
		json.RawMessage(`{
  "type":"object",
  "properties":{
    "query":{"type":"string","description":"Optional free-text filter over voice name, description, language, and characteristics"},
    "language":{"type":"string","description":"Optional ISO language filter such as en or hi"},
    "characteristic":{"type":"string","description":"Optional characteristic filter such as warm, calm, authoritative, or documentary"}
  }
}`),
	), env.listVoices)

	reg.Register(llm.NewFunctionTool(
		"generate_voiceover",
		"Generate a high-quality ElevenLabs voiceover. Use list_tts_voices first and provide a catalog-approved voice_id. Omit placement to create an asset only; provide placement to add the generated audio to A1 or A2 in the same project revision.",
		json.RawMessage(`{
  "type":"object",
  "properties":{
    "text":{"type":"string","description":"The exact narration or dialogue to speak"},
    "voice_id":{"type":"string","description":"Voice ID returned by list_tts_voices"},
    "model_id":{"type":"string","description":"Optional ElevenLabs TTS model override"},
    "language_code":{"type":"string","description":"Optional ISO 639-1 language code"},
    "output_format":{"type":"string","description":"Optional format such as mp3_44100_128"},
    "voice_settings":{"type":"object","description":"Optional ElevenLabs voice settings such as stability, similarity_boost, style, and use_speaker_boost"},
    "seed":{"type":"integer","description":"Optional best-effort deterministic seed"},
    "placement":{"type":"object","properties":{"at":{"type":"string","enum":["end","start","playhead"]},"start_frame":{"type":"integer","minimum":0},"track":{"type":"string","enum":["A1","A2"]}}}
  },
  "required":["text","voice_id"]
}`),
	), env.generateVoiceover)

	reg.Register(llm.NewFunctionTool(
		"generate_music",
		"Generate music with Gemini Lyria 3. Use a specific prompt describing genre, instruments, mood, BPM, key, structure, and intended duration. Use lyria-3-clip-preview for a 30-second clip or lyria-3-pro-preview for a longer structured song. Add 'instrumental only, no vocals' when dialogue or narration must remain clear. Omit placement to create an asset only; provide placement to add the generated audio to A1 or A2.",
		json.RawMessage(`{
  "type":"object",
  "properties":{
	"prompt":{"type":"string","description":"Detailed natural-language music direction. Include genre, instruments, BPM, key, mood, structure, duration, and lyrics or instrumental intent."},
	"model_id":{"type":"string","enum":["lyria-3-clip-preview","lyria-3-pro-preview"],"description":"Clip generates a fixed 30-second clip; Pro generates a longer structured song."},
	"duration_seconds":{"type":"number","minimum":1,"maximum":600,"description":"Target duration added to the prompt. Clip must use 30 seconds; Pro accepts an intended duration."},
	"force_instrumental":{"type":"boolean","description":"Append an instrumental-only instruction so the result has no vocals."},
	"output_format":{"type":"string","enum":["mp3","wav"],"description":"MP3 by default; WAV is supported by lyria-3-pro-preview."},
	"placement":{"type":"object","properties":{"at":{"type":"string","enum":["end","start","playhead"]},"start_frame":{"type":"integer","minimum":0},"track":{"type":"string","enum":["A1","A2"]}}}
	},
	"required":["prompt"]
}`),
	), env.generateMusic)

	reg.Register(llm.NewFunctionTool(
		"generate_sound_effect",
		"Generate an ElevenLabs sound effect from a precise natural-language description. Use duration_seconds for exact timing, loop for seamless ambience, and prompt_influence to control literalness. Omit placement to create an asset only.",
		json.RawMessage(`{
  "type":"object",
  "properties":{
    "text":{"type":"string","description":"Sound description such as cinematic thunder rolling behind a distant city"},
    "duration_seconds":{"type":"number","minimum":0.5,"maximum":30},
    "loop":{"type":"boolean"},
    "prompt_influence":{"type":"number","minimum":0,"maximum":1},
    "model_id":{"type":"string"},
    "output_format":{"type":"string"},
    "placement":{"type":"object","properties":{"at":{"type":"string","enum":["end","start","playhead"]},"start_frame":{"type":"integer","minimum":0},"track":{"type":"string","enum":["A1","A2"]}}}
  },
  "required":["text"]
}`),
	), env.generateSoundEffect)
}

func (e AudioGenerationEnv) listVoices(_ context.Context, raw json.RawMessage) Result {
	var in struct {
		Query          string `json:"query"`
		Language       string `json:"language"`
		Characteristic string `json:"characteristic"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	voices := e.Voices.List(in.Query, in.Language, in.Characteristic)
	return Result{OK: true, Output: map[string]any{"count": len(voices), "voices": voices}}
}

func (e AudioGenerationEnv) generateVoiceover(ctx context.Context, raw json.RawMessage) Result {
	var in struct {
		Text          string          `json:"text"`
		VoiceID       string          `json:"voice_id"`
		ModelID       string          `json:"model_id"`
		LanguageCode  string          `json:"language_code"`
		OutputFormat  string          `json:"output_format"`
		VoiceSettings map[string]any  `json:"voice_settings"`
		Seed          *int64          `json:"seed"`
		Placement     *audioPlacement `json:"placement"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if strings.TrimSpace(in.Text) == "" {
		return Result{OK: false, Error: "text is required"}
	}
	voice, err := e.Voices.Get(in.VoiceID)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	in.VoiceID = voice.ID
	if err := e.requireReady(); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if len([]rune(in.Text)) > 40000 {
		return Result{OK: false, Error: "text is too long; split voiceover into shorter generations"}
	}
	if err := e.Limiter.acquire(ctx); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	defer e.Limiter.release()
	response, err := e.Client.GenerateSpeechWithTimestamps(ctx, elevenlabs.SpeechRequest{
		VoiceID: in.VoiceID, Text: in.Text, ModelID: first(in.ModelID, e.TTSModel), LanguageCode: in.LanguageCode,
		VoiceSettings: in.VoiceSettings, Seed: in.Seed,
	}, first(in.OutputFormat, e.TTSOutputFormat))
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	audio, err := elevenlabs.DecodeAudioBase64(response.AudioBase64, e.Client.MaxResponseBytes)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	outputFormat := first(in.OutputFormat, e.TTSOutputFormat)
	ext := audioExtension(outputFormat, ".mp3")
	path, err := e.saveGenerated("voiceover", ext, audio)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	alignment := response.Alignment
	if alignment == nil {
		alignment = response.NormalizedAlignment
	}
	doc := transcriptFromAlignment(in.Text, in.LanguageCode, alignment)
	meta := transcript.GeneratedAudioMetadata{
		GenerationType: "voiceover", Prompt: in.Text, VoiceID: voice.ID, VoiceName: voice.Name,
		Characteristics: voice.Characteristics, Model: first(in.ModelID, e.TTSModel), Description: voice.Description,
	}
	if err := e.saveMetadata(path, meta); err != nil {
		_ = os.Remove(filepath.Join(e.Workspace, filepath.FromSlash(path)))
		return Result{OK: false, Error: err.Error()}
	}
	e.markMutation()
	e.scheduleIndex(path, doc, meta)
	return e.finishGenerated(ctx, path, in.Placement, map[string]any{"type": "voiceover", "voice": voice, "model": meta.Model})
}

func (e AudioGenerationEnv) generateMusic(ctx context.Context, raw json.RawMessage) Result {
	var in struct {
		Prompt            string          `json:"prompt"`
		ModelID           string          `json:"model_id"`
		DurationSeconds   *float64        `json:"duration_seconds"`
		ForceInstrumental bool            `json:"force_instrumental"`
		OutputFormat      string          `json:"output_format"`
		Placement         *audioPlacement `json:"placement"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if err := e.requireMusicReady(); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	in.Prompt = strings.TrimSpace(in.Prompt)
	model := first(in.ModelID, e.GeminiMusicModel)
	if model == "" {
		model = gemini.DefaultMusicModel
	}
	prompt, err := buildGeminiMusicPrompt(in.Prompt, model, in.DurationSeconds, in.ForceInstrumental)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if err := e.Limiter.acquire(ctx); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	defer e.Limiter.release()
	outputFormat := first(in.OutputFormat, e.GeminiMusicOutputFormat)
	result, err := e.MusicClient.GenerateMusic(ctx, gemini.MusicRequest{Model: model, Prompt: prompt, OutputFormat: outputFormat})
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	path, err := e.saveGenerated("music", audioExtension(outputFormat, ".mp3"), result.Audio)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	meta := transcript.GeneratedAudioMetadata{GenerationType: "music", Prompt: in.Prompt, Model: model, Lyrics: result.Lyrics, SongID: result.InteractionID}
	if err := e.saveMetadata(path, meta); err != nil {
		_ = os.Remove(filepath.Join(e.Workspace, filepath.FromSlash(path)))
		return Result{OK: false, Error: err.Error()}
	}
	e.markMutation()
	e.scheduleIndex(path, nil, meta)
	return e.finishGenerated(ctx, path, in.Placement, map[string]any{"type": "music", "model": meta.Model, "interaction_id": result.InteractionID, "lyrics": result.Lyrics})
}

func (e AudioGenerationEnv) generateSoundEffect(ctx context.Context, raw json.RawMessage) Result {
	var in struct {
		Text            string          `json:"text"`
		DurationSeconds *float64        `json:"duration_seconds"`
		Loop            bool            `json:"loop"`
		PromptInfluence *float64        `json:"prompt_influence"`
		ModelID         string          `json:"model_id"`
		OutputFormat    string          `json:"output_format"`
		Placement       *audioPlacement `json:"placement"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if err := e.requireReady(); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if strings.TrimSpace(in.Text) == "" {
		return Result{OK: false, Error: "text is required"}
	}
	if in.DurationSeconds != nil && (*in.DurationSeconds < 0.5 || *in.DurationSeconds > 30) {
		return Result{OK: false, Error: "duration_seconds must be between 0.5 and 30"}
	}
	if in.PromptInfluence != nil && (*in.PromptInfluence < 0 || *in.PromptInfluence > 1) {
		return Result{OK: false, Error: "prompt_influence must be between 0 and 1"}
	}
	if err := e.Limiter.acquire(ctx); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	defer e.Limiter.release()
	tmp, err := e.newTemp("sfx", ".mp3")
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	defer os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	outputFormat := first(in.OutputFormat, e.SFXOutputFormat)
	result, callErr := e.Client.GenerateSoundEffect(ctx, elevenlabs.SoundEffectRequest{
		Text: in.Text, DurationSeconds: in.DurationSeconds, Loop: in.Loop, PromptInfluence: in.PromptInfluence,
		ModelID: first(in.ModelID, e.SFXModel),
	}, outputFormat, f)
	closeErr := f.Close()
	if callErr != nil {
		return Result{OK: false, Error: callErr.Error()}
	}
	if closeErr != nil {
		return Result{OK: false, Error: closeErr.Error()}
	}
	path, err := e.moveTempToMedia(tmp, "sound-effect", audioExtension(outputFormat, ".mp3"))
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	meta := transcript.GeneratedAudioMetadata{GenerationType: "sound_effect", Prompt: in.Text, Description: in.Text, Model: first(in.ModelID, e.SFXModel)}
	if err := e.saveMetadata(path, meta); err != nil {
		_ = os.Remove(filepath.Join(e.Workspace, filepath.FromSlash(path)))
		return Result{OK: false, Error: err.Error()}
	}
	e.markMutation()
	e.scheduleIndex(path, nil, meta)
	return e.finishGenerated(ctx, path, in.Placement, map[string]any{"type": "sound_effect", "model": meta.Model, "bytes": result.Bytes})
}

func (e AudioGenerationEnv) requireReady() error {
	if e.Transaction == nil {
		return errors.New("audio generation requires a project timeline transaction")
	}
	if e.Client == nil {
		return errors.New("ElevenLabs is not configured")
	}
	return nil
}

func (e AudioGenerationEnv) requireMusicReady() error {
	if e.Transaction == nil {
		return errors.New("audio generation requires a project timeline transaction")
	}
	if e.MusicClient == nil {
		return errors.New("Gemini music is not configured; set GEMINI_API_KEY on the server")
	}
	return nil
}

func buildGeminiMusicPrompt(prompt, model string, duration *float64, instrumental bool) (string, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "", errors.New("prompt is required")
	}
	if duration != nil {
		if *duration < 1 || *duration > 600 {
			return "", errors.New("duration_seconds must be between 1 and 600")
		}
		if strings.TrimSpace(model) == "lyria-3-clip-preview" && *duration != 30 {
			return "", errors.New("lyria-3-clip-preview always generates a 30-second clip")
		}
	}
	if strings.TrimSpace(model) == "lyria-3-clip-preview" && duration == nil {
		prompt += "\nTarget duration: 30 seconds."
	} else if duration != nil {
		prompt += fmt.Sprintf("\nTarget duration: %.0f seconds.", *duration)
	}
	if instrumental && !strings.Contains(strings.ToLower(prompt), "instrumental") {
		prompt += "\nInstrumental only, no vocals."
	}
	return prompt, nil
}

func (e AudioGenerationEnv) finishGenerated(ctx context.Context, path string, placement *audioPlacement, details map[string]any) Result {
	out := map[string]any{"path": path, "media_type": "audio", "index_state": "queued"}
	for key, value := range details {
		out[key] = value
	}
	if placement == nil {
		return Result{OK: true, Output: out}
	}
	created, err := e.placeAudio(ctx, path, *placement)
	if err != nil {
		out["placement_error"] = err.Error()
		return Result{OK: false, Output: out, Error: "generated audio saved, but timeline placement failed: " + err.Error()}
	}
	out["placed"] = true
	out["created_ids"] = created
	return Result{OK: true, Output: out}
}

func (e AudioGenerationEnv) placeAudio(ctx context.Context, rel string, placement audioPlacement) ([]string, error) {
	track := strings.TrimSpace(placement.Track)
	if track == "" {
		track = "A1"
	}
	if track != "A1" && track != "A2" {
		return nil, errors.New("placement.track must be A1 or A2")
	}
	info, err := ffmpeg.ProbeMedia(ctx, e.Bins, e.Workspace, rel)
	if err != nil {
		return nil, fmt.Errorf("probe generated audio: %w", err)
	}
	doc := e.Transaction.Get()
	fps := doc.FPS
	if fps < 1 {
		fps = 24
	}
	start := projects.TimelineEndFrame(doc)
	switch strings.ToLower(strings.TrimSpace(placement.At)) {
	case "", "end":
	case "start":
		start = 0
	case "playhead":
		start = doc.PlayheadFrame
	default:
		return nil, errors.New("placement.at must be end, start, or playhead")
	}
	if placement.StartFrame != nil {
		if *placement.StartFrame < 0 {
			return nil, errors.New("placement.start_frame must be non-negative")
		}
		start = *placement.StartFrame
	}
	duration := projects.SecondsToFrames(info.Duration, fps)
	if duration < 1 {
		duration = 1
	}
	clips := projects.PlaceMediaClips(projects.MediaLayout{
		Path: rel, StartFrame: start, DurationFrames: duration, SourceDurationFrames: duration, HasAudio: true,
	})
	if len(clips) != 1 {
		return nil, errors.New("generated audio did not produce one audio clip")
	}
	clips[0].Track = track
	result, err := e.Transaction.Apply([]projects.TimelineOperation{{Type: "add_item", Item: &clips[0]}})
	if err != nil {
		return nil, err
	}
	e.Transaction.Focus(clips[0].ID, start)
	return result.CreatedIDs, nil
}

func (e AudioGenerationEnv) scheduleIndex(rel string, doc *transcript.Document, metadata transcript.GeneratedAudioMetadata) {
	if e.Indexer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	go func() {
		defer cancel()
		if err := e.Indexer.IndexGeneratedAudio(ctx, e.ProjectID, rel, doc, metadata); err != nil {
			e.log().Error("index generated audio", "project", e.ProjectID, "path", rel, "err", err)
		}
	}()
}

func (e AudioGenerationEnv) markMutation() {
	if e.OnMutation != nil {
		e.OnMutation()
	}
}

func (e AudioGenerationEnv) saveGenerated(kind, ext string, data []byte) (string, error) {
	generatedPathMu.Lock()
	defer generatedPathMu.Unlock()
	path, err := e.nextMediaPath(kind, ext)
	if err != nil {
		return "", err
	}
	if err := atomicWrite(path, data); err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Join("media", filepath.Base(path))), nil
}

func (e AudioGenerationEnv) moveTempToMedia(temp, kind, ext string) (string, error) {
	generatedPathMu.Lock()
	defer generatedPathMu.Unlock()
	path, err := e.nextMediaPath(kind, ext)
	if err != nil {
		return "", err
	}
	if err := os.Rename(temp, path); err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Join("media", filepath.Base(path))), nil
}

func (e AudioGenerationEnv) nextMediaPath(kind, ext string) (string, error) {
	if strings.TrimSpace(e.Workspace) == "" {
		return "", errors.New("project workspace is not configured")
	}
	dir := filepath.Join(e.Workspace, "media")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	base := fmt.Sprintf("%s-%d", kind, time.Now().UnixNano())
	for i := 0; i < 10000; i++ {
		name := base + ext
		if i > 0 {
			name = fmt.Sprintf("%s-%d%s", base, i, ext)
		}
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return path, nil
		}
	}
	return "", errors.New("could not allocate generated media filename")
}

func (e AudioGenerationEnv) newTemp(kind, ext string) (string, error) {
	if err := os.MkdirAll(filepath.Join(e.Workspace, ".scratch"), 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Join(e.Workspace, ".scratch"), kind+"-*"+ext)
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		return "", err
	}
	return name, nil
}

func (e AudioGenerationEnv) saveMetadata(rel string, metadata transcript.GeneratedAudioMetadata) error {
	dir := filepath.Join(e.Workspace, ".parallax", "generated-audio")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	name := strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel)) + ".json"
	return atomicWrite(filepath.Join(dir, name), data)
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".generated-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func transcriptFromAlignment(text, language string, alignment *elevenlabs.Alignment) *transcript.Document {
	doc := &transcript.Document{Language: strings.TrimSpace(language), Words: []transcript.Word{}, Segments: []transcript.Segment{}}
	if doc.Language == "" {
		doc.Language = "en"
	}
	if alignment == nil || len(alignment.Characters) == 0 {
		doc.Segments = []transcript.Segment{{ID: "seg-0000", Start: 0, End: 0, Text: strings.TrimSpace(text)}}
		return doc
	}
	var word strings.Builder
	var wordStart, wordEnd float64
	var haveWord bool
	var words []transcript.Word
	for i, char := range alignment.Characters {
		if i >= len(alignment.CharacterStartTimes) || i >= len(alignment.CharacterEndTimes) {
			break
		}
		start, end := alignment.CharacterStartTimes[i], alignment.CharacterEndTimes[i]
		runes := []rune(char)
		if len(runes) == 0 {
			continue
		}
		if unicode.IsSpace(runes[0]) {
			if haveWord {
				words = append(words, transcript.Word{Start: wordStart, End: wordEnd, Text: word.String()})
				word.Reset()
				haveWord = false
			}
			continue
		}
		if !haveWord {
			wordStart, haveWord = start, true
		}
		word.WriteString(char)
		wordEnd = end
	}
	if haveWord {
		words = append(words, transcript.Word{Start: wordStart, End: wordEnd, Text: word.String()})
	}
	doc.Words = words
	var current []transcript.Word
	for _, item := range words {
		current = append(current, item)
		last := item.Text[len(item.Text)-1]
		if last == '.' || last == '?' || last == '!' || last == ';' || len(current) >= 40 {
			appendSegment(doc, current)
			current = nil
		}
	}
	appendSegment(doc, current)
	if len(doc.Segments) == 0 {
		doc.Segments = []transcript.Segment{{ID: "seg-0000", Start: 0, End: 0, Text: strings.TrimSpace(text)}}
	}
	return doc
}

func appendSegment(doc *transcript.Document, words []transcript.Word) {
	if len(words) == 0 {
		return
	}
	parts := make([]string, 0, len(words))
	for _, word := range words {
		parts = append(parts, word.Text)
	}
	doc.Segments = append(doc.Segments, transcript.Segment{
		ID: fmt.Sprintf("seg-%04d", len(doc.Segments)), Start: words[0].Start, End: words[len(words)-1].End,
		Text: strings.Join(parts, " "),
	})
}

func compositionLyrics(raw json.RawMessage) string {
	var plan struct {
		Chunks []struct {
			Text  string   `json:"text"`
			Lines []string `json:"lines"`
		} `json:"chunks"`
		Sections []struct {
			Lines []string `json:"lines"`
		} `json:"sections"`
	}
	if json.Unmarshal(raw, &plan) != nil {
		return ""
	}
	var parts []string
	for _, chunk := range plan.Chunks {
		if strings.TrimSpace(chunk.Text) != "" {
			parts = append(parts, chunk.Text)
		}
		parts = append(parts, chunk.Lines...)
	}
	for _, section := range plan.Sections {
		parts = append(parts, section.Lines...)
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func audioExtension(format, fallback string) string {
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "" || format == "auto" {
		return fallback
	}
	for _, ext := range []string{".mp3", ".wav", ".m4a", ".flac", ".ogg", ".opus"} {
		if strings.HasPrefix(format, strings.TrimPrefix(ext, ".")) {
			return ext
		}
	}
	return fallback
}

func first(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(fallback)
}

func (e AudioGenerationEnv) log() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}
