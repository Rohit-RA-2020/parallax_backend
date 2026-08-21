package ffmpeg

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	previewMaxWidth = 1280
	previewTimeout  = 4 * time.Hour
)

// PreviewProgress reports how far a preview transcode has gotten.
type PreviewProgress func(at, duration float64)

// PreviewEncodeInfo identifies the encoder that produced a playback proxy.
type PreviewEncodeInfo struct {
	Encoder  string
	Device   string
	Hardware bool
	Pipeline string
}

// PreviewEncodePlan reports the encoder selected for a playback proxy before
// FFmpeg runs. The completed result may differ if hardware encoding falls back.
func PreviewEncodePlan(bins Bins) PreviewEncodeInfo {
	if bins.Accel.Enabled() {
		return PreviewEncodeInfo{
			Encoder:  bins.Accel.H264,
			Device:   previewDeviceLabel(bins.Accel),
			Hardware: true,
			Pipeline: previewHardwarePipeline(bins.Accel),
		}
	}
	return PreviewEncodeInfo{Encoder: "libx264", Device: "CPU", Pipeline: "cpu"}
}

// BrowserPlayable is true when a typical Chromium/Firefox <video> tag can
// decode this file without a proxy. MKV, HEVC, and 10-bit sources are not.
func BrowserPlayable(rel string, info MediaProbe) bool {
	if !info.HasVideo {
		return false
	}
	ext := strings.ToLower(filepath.Ext(rel))
	codec := normalizeVideoCodec(info.VideoCodec)
	if highBitDepth(info.PixelFormat) {
		return false
	}
	switch ext {
	case ".mp4", ".m4v", ".mov":
		return codec == "h264"
	case ".webm":
		switch codec {
		case "vp8", "vp9", "av1":
			return true
		default:
			return false
		}
	default:
		return false
	}
}

func normalizeVideoCodec(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "h264", "avc", "avc1", "avc3":
		return "h264"
	case "hevc", "h265", "h.265", "hev1", "hvc1":
		return "hevc"
	case "mpeg4", "mp4v":
		return "mpeg4"
	default:
		return strings.ToLower(strings.TrimSpace(name))
	}
}

func highBitDepth(pix string) bool {
	p := strings.ToLower(pix)
	return strings.Contains(p, "p10") || strings.Contains(p, "p12") || strings.Contains(p, "p16") ||
		strings.Contains(p, "10le") || strings.Contains(p, "12le") || strings.Contains(p, "16le")
}

// PreviewReason explains why a browser cannot play the source as-is.
func PreviewReason(rel string, info MediaProbe) string {
	if BrowserPlayable(rel, info) {
		return ""
	}
	ext := strings.ToLower(filepath.Ext(rel))
	codec := normalizeVideoCodec(info.VideoCodec)
	switch {
	case ext == ".mkv" || ext == ".avi" || ext == ".ts" || ext == ".mts":
		if codec != "" {
			return fmt.Sprintf("%s / %s is not playable in the browser", strings.TrimPrefix(ext, "."), codec)
		}
		return fmt.Sprintf("%s is not playable in the browser", strings.TrimPrefix(ext, "."))
	case codec == "hevc":
		if highBitDepth(info.PixelFormat) {
			return "10-bit HEVC is not playable in the browser"
		}
		return "HEVC is not playable in this browser"
	case highBitDepth(info.PixelFormat):
		return "10-bit video is not playable in the browser"
	case codec != "" && codec != "h264":
		return codec + " is not playable in the browser"
	default:
		return "this file needs a browser preview"
	}
}

// WritePreview transcodes a browser-safe H.264/AAC MP4 (max 1280px wide, 8-bit).
func WritePreview(ctx context.Context, bins Bins, workspace, inRel, outRel string, duration float64, onProgress PreviewProgress) error {
	_, err := WritePreviewWithInfo(ctx, bins, workspace, inRel, outRel, duration, onProgress)
	return err
}

// WritePreviewWithInfo transcodes a preview and returns the encoder that
// actually succeeded, including any CPU fallback performed by Run.
func WritePreviewWithInfo(ctx context.Context, bins Bins, workspace, inRel, outRel string, duration float64, onProgress PreviewProgress) (PreviewEncodeInfo, error) {
	if err := os.MkdirAll(filepath.Join(workspace, filepath.Dir(filepath.FromSlash(outRel))), 0o755); err != nil {
		return PreviewEncodeInfo{}, err
	}
	scratchDir := filepath.Join(workspace, ".scratch")
	if err := os.MkdirAll(scratchDir, 0o755); err != nil {
		return PreviewEncodeInfo{}, err
	}
	prog, err := os.CreateTemp(scratchDir, "preview-progress-*")
	if err != nil {
		return PreviewEncodeInfo{}, err
	}
	progName := prog.Name()
	_ = prog.Close()
	defer os.Remove(progName)
	progRel, err := filepath.Rel(workspace, progName)
	if err != nil {
		progRel = filepath.ToSlash(filepath.Join(".scratch", filepath.Base(progName)))
	} else {
		progRel = filepath.ToSlash(progRel)
	}

	args := []string{
		"ffmpeg", "-y",
		"-i", inRel,
		"-map", "0:v:0",
		"-map", "0:a:0?",
		"-vf", fmt.Sprintf("scale=w='min(%d,iw)':h=-2,format=yuv420p", previewMaxWidth),
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "23",
		"-pix_fmt", "yuv420p",
		"-c:a", "aac",
		"-ac", "2",
		"-b:a", "160k",
		"-movflags", "+faststart",
		"-progress", progRel,
		"-nostats",
		outRel,
	}
	cmd, err := Validate(args, ValidateOpts{Workspace: workspace})
	if err != nil {
		return PreviewEncodeInfo{}, fmt.Errorf("preview encode: %w", err)
	}

	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, runErr := runPreview(ctx, bins, cmd, workspace)
		done <- outcome{result: result, err: runErr}
	}()

	tick := time.NewTicker(400 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case finished := <-done:
			if finished.err != nil && strings.Contains(finished.err.Error(), "does not contain any stream") {
				return writePreviewVideoOnly(ctx, bins, workspace, inRel, outRel, progRel, duration, onProgress)
			}
			return previewEncodeResult(bins, finished.result), finished.err
		case <-tick.C:
			if onProgress == nil {
				continue
			}
			if at, ok := readProgressTime(filepath.Join(workspace, filepath.FromSlash(progRel))); ok {
				onProgress(at, duration)
			}
		case <-ctx.Done():
			return PreviewEncodeInfo{}, ctx.Err()
		}
	}
}

func writePreviewVideoOnly(ctx context.Context, bins Bins, workspace, inRel, outRel, progRel string, duration float64, onProgress PreviewProgress) (PreviewEncodeInfo, error) {
	cmd, err := Validate([]string{
		"ffmpeg", "-y",
		"-i", inRel,
		"-map", "0:v:0",
		"-an",
		"-vf", fmt.Sprintf("scale=w='min(%d,iw)':h=-2,format=yuv420p", previewMaxWidth),
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "23",
		"-pix_fmt", "yuv420p",
		"-movflags", "+faststart",
		"-progress", progRel,
		"-nostats",
		outRel,
	}, ValidateOpts{Workspace: workspace})
	if err != nil {
		return PreviewEncodeInfo{}, fmt.Errorf("preview encode: %w", err)
	}
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, runErr := runPreview(ctx, bins, cmd, workspace)
		done <- outcome{result: result, err: runErr}
	}()
	tick := time.NewTicker(400 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case finished := <-done:
			return previewEncodeResult(bins, finished.result), finished.err
		case <-tick.C:
			if onProgress == nil {
				continue
			}
			if at, ok := readProgressTime(filepath.Join(workspace, filepath.FromSlash(progRel))); ok {
				onProgress(at, duration)
			}
		case <-ctx.Done():
			return PreviewEncodeInfo{}, ctx.Err()
		}
	}
}

func runPreview(ctx context.Context, bins Bins, cmd Command, workspace string) (Result, error) {
	if cudaCmd, ok := cudaPreviewCommand(cmd, bins.Accel); ok {
		cudaBins := bins
		cudaBins.Accel = Accel{}
		gpu, err := Run(ctx, cudaBins, cudaCmd, workspace, previewTimeout)
		if err == nil {
			return gpu, nil
		}
		if ctx.Err() != nil {
			return gpu, err
		}
		fallback, fallbackErr := Run(ctx, bins, cmd, workspace, previewTimeout)
		if fallbackErr == nil {
			fallback.Stderr = "full gpu preview failed; retried with fallback pipeline\n" + lastLines(gpu.Stderr, 8) + "\n" + fallback.Stderr
			return fallback, nil
		}
		return gpu, fmt.Errorf("%w (preview fallback also failed: %v)", err, fallbackErr)
	}
	return Run(ctx, bins, cmd, workspace, previewTimeout)
}

func cudaPreviewCommand(cmd Command, accel Accel) (Command, bool) {
	if cmd.Kind != KindFFmpeg || accel.Backend != "cuda" || !accel.Enabled() {
		return Command{}, false
	}
	args, ok := RewriteSoftwareEncode(cmd.Args, accel)
	if !ok {
		return Command{}, false
	}
	out := make([]string, 0, len(args)+6)
	insertedInput := false
	for i := 0; i < len(args); i++ {
		name, inline := splitFlag(args[i])
		if name == "-i" && !insertedInput {
			out = append(out, "-hwaccel", "cuda", "-hwaccel_output_format", "cuda")
			if device := strings.TrimSpace(accel.Device); device != "" {
				out = append(out, "-hwaccel_device", device)
			}
			insertedInput = true
		}
		if name == "-vf" || name == "-filter" {
			out = append(out, name, fmt.Sprintf("scale_cuda=w='min(%d,iw)':h=-2:format=yuv420p:interp_algo=bicubic", previewMaxWidth))
			if inline == "" && i+1 < len(args) {
				i++
			}
			continue
		}
		if name == "-pix_fmt" || name == "-pix_fmt:v" {
			if inline == "" && i+1 < len(args) {
				i++
			}
			continue
		}
		out = append(out, args[i])
	}
	if !insertedInput {
		return Command{}, false
	}
	return Command{Kind: cmd.Kind, Args: out}, true
}

func previewEncodeResult(bins Bins, result Result) PreviewEncodeInfo {
	encoder := ""
	for i := 0; i+1 < len(result.Args); i++ {
		if result.Args[i] == "-c:v" || result.Args[i] == "-codec:v" {
			encoder = strings.TrimSpace(result.Args[i+1])
		}
	}
	if encoder == "" {
		return PreviewEncodeInfo{}
	}
	hardware := isHWEncoderName(encoder)
	device := "CPU"
	if hardware {
		device = previewDeviceLabel(bins.Accel)
	}
	pipeline := "cpu"
	if hardware {
		pipeline = "gpu_encode"
		if hasFlag(result.Args, "-hwaccel_output_format") && containsArgValue(result.Args, "cuda") && strings.Contains(strings.Join(result.Args, ","), "scale_cuda=") {
			pipeline = "gpu_full"
		}
	}
	return PreviewEncodeInfo{Encoder: encoder, Device: device, Hardware: hardware, Pipeline: pipeline}
}

func containsArgValue(args []string, want string) bool {
	for _, arg := range args {
		if strings.EqualFold(strings.TrimSpace(arg), want) {
			return true
		}
	}
	return false
}

func previewHardwarePipeline(accel Accel) string {
	if accel.Backend == "cuda" {
		return "gpu_full"
	}
	return "gpu_encode"
}

func previewDeviceLabel(accel Accel) string {
	label := strings.TrimSpace(accel.Label)
	if label == "" {
		label = strings.ToUpper(strings.TrimSpace(accel.Backend))
	}
	if label == "" {
		label = "GPU"
	}
	if device := strings.TrimSpace(accel.Device); device != "" {
		return fmt.Sprintf("%s (%s)", label, device)
	}
	return label
}

func readProgressTime(path string) (float64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	var last float64
	ok := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		key, val, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch key {
		case "out_time_ms":
			if n, err := strconv.ParseFloat(val, 64); err == nil {
				last = n / 1000
				ok = true
			}
		case "out_time_us":
			if n, err := strconv.ParseFloat(val, 64); err == nil {
				last = n / 1e6
				ok = true
			}
		case "out_time":
			if sec, parsed := parseClock(val); parsed {
				last = sec
				ok = true
			}
		}
	}
	return last, ok
}

func parseClock(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, false
	}
	h, errH := strconv.ParseFloat(parts[0], 64)
	m, errM := strconv.ParseFloat(parts[1], 64)
	sec, errS := strconv.ParseFloat(parts[2], 64)
	if errH != nil || errM != nil || errS != nil {
		return 0, false
	}
	return h*3600 + m*60 + sec, true
}
