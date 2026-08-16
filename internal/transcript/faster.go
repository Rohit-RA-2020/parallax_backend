package transcript

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// FasterWhisper talks to scripts/transcribe.py, preferring a resident worker.
type FasterWhisper struct {
	Python  string
	Script  string
	Model   string
	Device  string
	Compute string

	mu     sync.Mutex
	cmd    *exec.Cmd
	port   int
	client *http.Client
}

type fasterPayload struct {
	Type     string    `json:"type"`
	OK       bool      `json:"ok"`
	Error    string    `json:"error"`
	Language string    `json:"language"`
	Model    string    `json:"model"`
	Device   string    `json:"device"`
	At       float64   `json:"at"`
	Duration float64   `json:"duration"`
	Segments []Segment `json:"segments"`
	Words    []Word    `json:"words"`
}

func (w *FasterWhisper) Transcribe(ctx context.Context, wavPath string, progress ProgressFunc) (ASRResult, error) {
	if err := w.Ensure(ctx); err == nil {
		res, err := w.httpTranscribe(ctx, wavPath, progress)
		if err == nil {
			return res, nil
		}
		w.mu.Lock()
		w.stopLocked()
		w.mu.Unlock()
	}
	return w.oneShot(ctx, wavPath)
}

// Ensure starts the resident worker and loads the model once.
func (w *FasterWhisper) Ensure(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.healthy() {
		return nil
	}
	w.stopLocked()
	python := strings.TrimSpace(w.Python)
	if python == "" {
		python = "python3"
	}
	script := strings.TrimSpace(w.Script)
	if script == "" {
		return fmt.Errorf("faster-whisper: script path is not set")
	}
	args := []string{script, "serve", "--model", firstNonEmpty(w.Model, "large-v3-turbo")}
	if d := strings.TrimSpace(w.Device); d != "" {
		args = append(args, "--device", d)
	}
	if c := strings.TrimSpace(w.Compute); c != "" {
		args = append(args, "--compute", c)
	}
	cmd := exec.Command(python, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("faster-whisper worker: %w", err)
	}
	type hello struct {
		OK   bool `json:"ok"`
		Port int  `json:"port"`
	}
	done := make(chan error, 1)
	var greet hello
	go func() {
		sc := bufio.NewScanner(stdout)
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				done <- err
				return
			}
			done <- fmt.Errorf("faster-whisper worker produced no ready line")
			return
		}
		done <- json.Unmarshal(sc.Bytes(), &greet)
	}()
	select {
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		return ctx.Err()
	case err := <-done:
		if err != nil {
			_ = cmd.Process.Kill()
			return fmt.Errorf("faster-whisper worker: %w", err)
		}
	case <-time.After(3 * time.Minute):
		_ = cmd.Process.Kill()
		return fmt.Errorf("faster-whisper worker timed out while loading the model")
	}
	if !greet.OK || greet.Port < 1 {
		_ = cmd.Process.Kill()
		return fmt.Errorf("faster-whisper worker did not publish a port")
	}
	w.cmd = cmd
	w.port = greet.Port
	w.client = &http.Client{Timeout: 45 * time.Minute}
	return nil
}

func (w *FasterWhisper) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopLocked()
}

func (w *FasterWhisper) healthy() bool {
	if w.cmd == nil || w.cmd.Process == nil || w.port < 1 || w.client == nil {
		return false
	}
	resp, err := w.client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", w.port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

func (w *FasterWhisper) stopLocked() {
	if w.cmd != nil && w.cmd.Process != nil {
		_ = w.cmd.Process.Kill()
		_, _ = w.cmd.Process.Wait()
	}
	w.cmd = nil
	w.port = 0
}

func (w *FasterWhisper) httpTranscribe(ctx context.Context, wavPath string, progress ProgressFunc) (ASRResult, error) {
	w.mu.Lock()
	port := w.port
	client := w.client
	w.mu.Unlock()
	if port < 1 || client == nil {
		return ASRResult{}, fmt.Errorf("faster-whisper worker is not running")
	}
	body, err := json.Marshal(map[string]string{"wav": wavPath})
	if err != nil {
		return ASRResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/transcribe", port), bytes.NewReader(body))
	if err != nil {
		return ASRResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return ASRResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return ASRResult{}, fmt.Errorf("faster-whisper worker: http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	var last fasterPayload
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev fasterPayload
		if err := json.Unmarshal(line, &ev); err != nil {
			return ASRResult{}, fmt.Errorf("faster-whisper worker json: %w", err)
		}
		switch ev.Type {
		case "progress":
			if progress != nil {
				progress(ev.At, ev.Duration)
			}
		default:
			last = ev
		}
	}
	if err := sc.Err(); err != nil {
		return ASRResult{}, err
	}
	return resultFromPayload(last, w.Model)
}

func (w *FasterWhisper) oneShot(ctx context.Context, wavPath string) (ASRResult, error) {
	python := strings.TrimSpace(w.Python)
	if python == "" {
		python = "python3"
	}
	script := strings.TrimSpace(w.Script)
	if script == "" {
		return ASRResult{}, fmt.Errorf("faster-whisper: script path is not set")
	}
	model := firstNonEmpty(w.Model, "large-v3-turbo")
	if _, err := os.Stat(wavPath); err != nil {
		return ASRResult{}, fmt.Errorf("faster-whisper input: %w", err)
	}
	args := []string{script, wavPath, "--model", model}
	if d := strings.TrimSpace(w.Device); d != "" {
		args = append(args, "--device", d)
	}
	if c := strings.TrimSpace(w.Compute); c != "" {
		args = append(args, "--compute", c)
	}
	cmd := exec.CommandContext(ctx, python, args...)
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
	return resultFromPayload(payload, model)
}

func resultFromPayload(payload fasterPayload, model string) (ASRResult, error) {
	if payload.Error != "" && !payload.OK && payload.Type != "progress" {
		return ASRResult{}, fmt.Errorf("faster-whisper: %s", payload.Error)
	}
	if !payload.OK && payload.Language == "" && len(payload.Segments) == 0 && payload.Error != "" {
		return ASRResult{}, fmt.Errorf("faster-whisper: %s", payload.Error)
	}
	out := ASRResult{
		Language: strings.ToLower(strings.TrimSpace(payload.Language)),
		Model:    firstNonEmpty(payload.Model, model),
		Words:    payload.Words,
		Segments: payload.Segments,
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
