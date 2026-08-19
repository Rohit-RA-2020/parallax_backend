package ffmpeg

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Accel is a process-selected GPU encode backend. The zero value is CPU-only.
type Accel struct {
	Backend string // cuda, vaapi, qsv, videotoolbox
	Device  string
	Label   string
	H264    string
	HEVC    string
	VP9     string
	AV1     string
}

// DetectOpts selects which backend to try.
type DetectOpts struct {
	// Prefer is auto (default), off, cuda, vaapi, qsv, or videotoolbox.
	Prefer string
	// Device is an optional GPU index (cuda) or render node (vaapi).
	Device string
}

// Enabled reports whether a usable H.264 hardware encoder was selected.
func (a Accel) Enabled() bool {
	return strings.TrimSpace(a.Backend) != "" && strings.TrimSpace(a.H264) != ""
}

func (a Accel) encoderFor(family string) string {
	switch family {
	case "h264":
		return a.H264
	case "hevc":
		return a.HEVC
	case "vp9":
		return a.VP9
	case "av1":
		return a.AV1
	}
	return ""
}

func (a Accel) String() string {
	if !a.Enabled() {
		return "off"
	}
	var encs []string
	for _, name := range []string{a.H264, a.HEVC, a.VP9, a.AV1} {
		if name != "" {
			encs = append(encs, name)
		}
	}
	label := a.Label
	if label == "" {
		label = a.Backend
	}
	if a.Device != "" {
		return fmt.Sprintf("%s (%s) [%s]", label, a.Device, strings.Join(encs, ", "))
	}
	return fmt.Sprintf("%s [%s]", label, strings.Join(encs, ", "))
}

// PromptNote is appended to the Director system prompt when GPU encode is on.
func (a Accel) PromptNote() string {
	if !a.Enabled() {
		return ""
	}
	return fmt.Sprintf("## FFmpeg GPU\n- Hardware encode is on (%s). Keep using libx264 / libx265 / libvpx-vp9 in run_ffmpeg; the sandbox rewrites those to the GPU encoder. Do not add -hwaccel or vendor flags unless the user asked.\n", a.String())
}

func (a Accel) needsUpload() bool {
	return a.Backend == "vaapi" || a.Backend == "qsv"
}

// DetectAccel probes ffmpeg plus the host GPUs and returns a backend that
// actually encoded a test frame. Failures stay on CPU.
func DetectAccel(ctx context.Context, bins Bins, opts DetectOpts) Accel {
	prefer := normalizeHWPrefer(opts.Prefer)
	if prefer == "off" {
		return Accel{}
	}
	encoders := listEncoders(ctx, bins)
	if len(encoders) == 0 {
		return Accel{}
	}

	order := []string{"cuda", "videotoolbox", "qsv", "vaapi"}
	if prefer != "auto" {
		order = []string{prefer}
	}
	for _, backend := range order {
		if a, ok := probeBackend(ctx, bins, backend, opts.Device, encoders); ok {
			return a
		}
	}
	return Accel{}
}

func normalizeHWPrefer(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "auto", "on", "true", "1", "hw":
		return "auto"
	case "off", "none", "cpu", "software", "false", "0":
		return "off"
	case "cuda", "nvenc", "nvidia":
		return "cuda"
	case "vaapi", "va":
		return "vaapi"
	case "qsv", "quick", "intel":
		return "qsv"
	case "videotoolbox", "vt", "macos":
		return "videotoolbox"
	default:
		return "auto"
	}
}

func probeBackend(ctx context.Context, bins Bins, backend, device string, encoders map[string]bool) (Accel, bool) {
	switch backend {
	case "cuda":
		if !nvidiaPresent(ctx) {
			return Accel{}, false
		}
		a := Accel{
			Backend: "cuda",
			Device:  strings.TrimSpace(device),
			Label:   nvidiaLabel(ctx),
			H264:    pickEncoder(encoders, "h264_nvenc"),
			HEVC:    pickEncoder(encoders, "hevc_nvenc"),
			AV1:     pickEncoder(encoders, "av1_nvenc"),
		}
		return confirmAccel(ctx, bins, a)
	case "videotoolbox":
		if runtime.GOOS != "darwin" {
			return Accel{}, false
		}
		a := Accel{
			Backend: "videotoolbox",
			Label:   "VideoToolbox",
			H264:    pickEncoder(encoders, "h264_videotoolbox"),
			HEVC:    pickEncoder(encoders, "hevc_videotoolbox"),
			VP9:     pickEncoder(encoders, "vp9_videotoolbox"),
			AV1:     pickEncoder(encoders, "av1_videotoolbox"),
		}
		return confirmAccel(ctx, bins, a)
	case "qsv":
		a := Accel{
			Backend: "qsv",
			Device:  strings.TrimSpace(device),
			Label:   "Intel Quick Sync",
			H264:    pickEncoder(encoders, "h264_qsv"),
			HEVC:    pickEncoder(encoders, "hevc_qsv"),
			VP9:     pickEncoder(encoders, "vp9_qsv"),
			AV1:     pickEncoder(encoders, "av1_qsv"),
		}
		return confirmAccel(ctx, bins, a)
	case "vaapi":
		devices := []string{}
		if strings.TrimSpace(device) != "" {
			devices = []string{strings.TrimSpace(device)}
		} else {
			devices = renderNodes()
		}
		for _, dev := range devices {
			a := Accel{
				Backend: "vaapi",
				Device:  dev,
				Label:   "VAAPI",
				H264:    pickEncoder(encoders, "h264_vaapi"),
				HEVC:    pickEncoder(encoders, "hevc_vaapi"),
				VP9:     pickEncoder(encoders, "vp9_vaapi"),
				AV1:     pickEncoder(encoders, "av1_vaapi"),
			}
			if confirmed, ok := confirmAccel(ctx, bins, a); ok {
				return confirmed, true
			}
		}
		return Accel{}, false
	}
	return Accel{}, false
}

func confirmAccel(ctx context.Context, bins Bins, a Accel) (Accel, bool) {
	if a.H264 == "" || !smokeEncode(ctx, bins, a, a.H264) {
		return Accel{}, false
	}
	if a.HEVC != "" && !smokeEncode(ctx, bins, a, a.HEVC) {
		a.HEVC = ""
	}
	if a.VP9 != "" && !smokeEncode(ctx, bins, a, a.VP9) {
		a.VP9 = ""
	}
	if a.AV1 != "" && !smokeEncode(ctx, bins, a, a.AV1) {
		a.AV1 = ""
	}
	return a, true
}

func pickEncoder(encoders map[string]bool, name string) string {
	if encoders[name] {
		return name
	}
	return ""
}

func listEncoders(ctx context.Context, bins Bins) map[string]bool {
	out, err := ffmpegOutput(ctx, bins, 8*time.Second, "-hide_banner", "-encoders")
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 || !isCodecFlags(fields[0]) {
			continue
		}
		set[fields[1]] = true
	}
	return set
}

func isCodecFlags(s string) bool {
	if len(s) < 5 || len(s) > 8 {
		return false
	}
	for _, r := range s {
		if r != '.' && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

func smokeEncode(ctx context.Context, bins Bins, a Accel, encoder string) bool {
	dir, err := os.MkdirTemp("", "parallax-ffmpeg-hw-")
	if err != nil {
		return false
	}
	defer os.RemoveAll(dir)

	dest := filepath.Join(dir, "smoke.mp4")
	// NVENC rejects frames below ~129x64 on L4 and other cards.
	args := []string{"-hide_banner", "-y", "-f", "lavfi", "-i", "color=c=black:s=256x256:d=0.04", "-frames:v", "1"}
	args = append(args, hwInitArgs(a)...)
	if vf := hwUploadFilter(a); vf != "" {
		args = append(args, "-vf", vf)
	}
	args = append(args, "-c:v", encoder)
	args = append(args, hwEncoderExtras(a)...)
	args = append(args, dest)

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bins.path(KindFFmpeg), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return false
	}
	st, err := os.Stat(dest)
	return err == nil && st.Size() > 0
}

func hwInitArgs(a Accel) []string {
	switch a.Backend {
	case "vaapi":
		dev := a.Device
		if dev == "" {
			dev = "/dev/dri/renderD128"
		}
		return []string{"-init_hw_device", "vaapi=va:" + dev, "-filter_hw_device", "va"}
	case "qsv":
		if a.Device != "" {
			return []string{"-init_hw_device", "qsv=hw:" + a.Device, "-filter_hw_device", "hw"}
		}
		return []string{"-init_hw_device", "qsv=hw", "-filter_hw_device", "hw"}
	}
	return nil
}

func hwUploadFilter(a Accel) string {
	switch a.Backend {
	case "vaapi":
		return "format=nv12,hwupload"
	case "qsv":
		return "format=nv12,hwupload=extra_hw_frames=64"
	}
	return ""
}

func hwEncoderExtras(a Accel) []string {
	if a.Backend == "cuda" && a.Device != "" {
		return []string{"-gpu", a.Device}
	}
	return nil
}

func nvidiaPresent(ctx context.Context) bool {
	if _, err := os.Stat("/dev/nvidia0"); err == nil {
		return true
	}
	if _, err := os.Stat("/dev/nvidiactl"); err == nil {
		return true
	}
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return false
	}
	out, err := commandOutput(ctx, 4*time.Second, "nvidia-smi", "-L")
	return err == nil && strings.Contains(out, "GPU")
}

func nvidiaLabel(ctx context.Context) string {
	out, err := commandOutput(ctx, 4*time.Second, "nvidia-smi", "--query-gpu=name", "--format=csv,noheader")
	if err != nil {
		return "NVIDIA"
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			names = append(names, line)
		}
	}
	if len(names) == 0 {
		return "NVIDIA"
	}
	if len(names) == 1 {
		return names[0]
	}
	return fmt.Sprintf("%s +%d", names[0], len(names)-1)
}

func renderNodes() []string {
	matches, err := filepath.Glob("/dev/dri/renderD*")
	if err != nil {
		return nil
	}
	sort.Strings(matches)
	return matches
}

func ffmpegOutput(ctx context.Context, bins Bins, timeout time.Duration, args ...string) (string, error) {
	return commandOutput(ctx, timeout, bins.path(KindFFmpeg), args...)
}

func commandOutput(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := stdout.String()
	if strings.TrimSpace(out) == "" {
		out = stderr.String()
	}
	return out, err
}
