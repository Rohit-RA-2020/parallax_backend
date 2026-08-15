package ffmpeg

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Bins holds resolved ffmpeg / ffprobe executables.
type Bins struct {
	FFmpeg  string
	FFprobe string
}

func (b Bins) path(kind Kind) string {
	switch kind {
	case KindFFprobe:
		if b.FFprobe != "" {
			return b.FFprobe
		}
		return "ffprobe"
	default:
		if b.FFmpeg != "" {
			return b.FFmpeg
		}
		return "ffmpeg"
	}
}

// Result is the captured outcome of one process.
type Result struct {
	Kind     Kind     `json:"kind"`
	Args     []string `json:"args"`
	ExitCode int      `json:"exit_code"`
	Stdout   string   `json:"stdout"`
	Stderr   string   `json:"stderr"`
	Duration string   `json:"duration"`
}

// Run executes a validated command with cmd.Dir set to the workspace.
// It never invokes a shell.
func Run(ctx context.Context, bins Bins, cmd Command, workspace string, timeout time.Duration) (Result, error) {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	bin := bins.path(cmd.Kind)
	c := exec.CommandContext(ctx, bin, cmd.Args...)
	c.Dir = workspace
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr

	start := time.Now()
	err := c.Run()
	res := Result{
		Kind:     cmd.Kind,
		Args:     cmd.Args,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start).Round(time.Millisecond).String(),
	}
	if c.ProcessState != nil {
		res.ExitCode = c.ProcessState.ExitCode()
	}
	if err != nil {
		if ctx.Err() != nil {
			return res, fmt.Errorf("%s timed out after %s: %s", cmd.Kind, timeout, lastLines(res.Stderr, 20))
		}
		return res, fmt.Errorf("%s exited %d: %s", cmd.Kind, res.ExitCode, lastLines(res.Stderr, 20))
	}
	return res, nil
}

func lastLines(s string, n int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(no stderr)"
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
