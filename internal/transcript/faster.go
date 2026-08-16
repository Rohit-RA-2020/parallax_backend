package transcript

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// FasterWhisper runs scripts/transcribe.py (faster-whisper).
type FasterWhisper struct {
	Python  string
	Script  string
	Model   string
	Device  string
	Compute string
}

type fasterPayload struct {
	OK       bool      `json:"ok"`
	Error    string    `json:"error"`
	Language string    `json:"language"`
	Model    string    `json:"model"`
	Device   string    `json:"device"`
	Segments []Segment `json:"segments"`
	Words    []Word    `json:"words"`
}

func (w FasterWhisper) Transcribe(ctx context.Context, wavPath string) (ASRResult, error) {
	python := strings.TrimSpace(w.Python)
	if python == "" {
		python = "python3"
	}
	script := strings.TrimSpace(w.Script)
	if script == "" {
		return ASRResult{}, fmt.Errorf("faster-whisper: script path is not set")
	}
	model := strings.TrimSpace(w.Model)
	if model == "" {
		model = "large-v3-turbo"
	}
	if _, err := os.Stat(wavPath); err != nil {
		return ASRResult{}, fmt.Errorf("faster-whisper input: %w", err)
	}
	if _, err := os.Stat(script); err != nil {
		return ASRResult{}, fmt.Errorf("faster-whisper script: %w", err)
	}

	args := []string{script, wavPath, "--model", model}
	if d := strings.TrimSpace(w.Device); d != "" {
		args = append(args, "--device", d)
	}
	if c := strings.TrimSpace(w.Compute); c != "" {
		args = append(args, "--compute", c)
	}

	cmd := exec.CommandContext(ctx, python, args...)
	cmd.Dir = filepath.Dir(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return ASRResult{}, fmt.Errorf("faster-whisper: %s", lastLines(msg, 30))
	}

	raw := bytes.TrimSpace(stdout.Bytes())
	if len(raw) == 0 {
		return ASRResult{}, fmt.Errorf("faster-whisper: empty output: %s", lastLines(stderr.String(), 20))
	}
	var payload fasterPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ASRResult{}, fmt.Errorf("faster-whisper json: %w", err)
	}
	if !payload.OK && payload.Error != "" {
		return ASRResult{}, fmt.Errorf("faster-whisper: %s", payload.Error)
	}
	out := ASRResult{
		Language: strings.ToLower(strings.TrimSpace(payload.Language)),
		Model:    payload.Model,
		Words:    payload.Words,
		Segments: payload.Segments,
	}
	if out.Model == "" {
		out.Model = model
	}
	if out.Words == nil {
		out.Words = []Word{}
	}
	if out.Segments == nil {
		out.Segments = []Segment{}
	}
	assignSegmentIDs(out.Segments)
	return out, nil
}
