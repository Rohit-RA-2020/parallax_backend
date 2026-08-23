package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kkdai/youtube/v2"

	"parallax/internal/ffmpeg"
	"parallax/internal/llm"
)

const (
	defaultYouTubeTimeout  = 30 * time.Minute
	defaultYouTubeMaxBytes = int64(8 << 30)
)

// YouTubeClient is the subset of kkdai/youtube used by the Director tool.
// Keeping the boundary small makes downloads testable without contacting YouTube.
type YouTubeClient interface {
	GetVideoContext(context.Context, string) (*youtube.Video, error)
	GetStreamContext(context.Context, *youtube.Video, *youtube.Format) (io.ReadCloser, int64, error)
}

// YouTubeEnv confines downloaded streams and merged output to one project.
type YouTubeEnv struct {
	Workspace  string
	Bins       ffmpeg.Bins
	Client     YouTubeClient
	YTDLPBin   string
	Timeout    time.Duration
	MaxBytes   int64
	OnMutation func()
	OnApplied  func(rel string)
}

func RegisterYouTube(reg *Registry, env YouTubeEnv) {
	reg.Register(llm.NewFunctionTool(
		"download_youtube_video",
		"Download one YouTube video that the user owns or has permission to use into the project media bin at a requested resolution. Use exact mode unless the user permits a lower fallback. The result reports the actual selected resolution and path. This tool does not download playlists, private videos, or DRM-protected content.",
		json.RawMessage(`{
			"type":"object",
			"properties":{
				"url":{"type":"string","description":"Full https://www.youtube.com/watch?v=... or https://youtu.be/... URL"},
				"resolution":{"type":"integer","minimum":144,"maximum":4320,"description":"Requested resolution in pixels, for example 720, 1080, 1440, or 2160"},
				"selection":{"type":"string","enum":["exact","at_most"],"description":"exact requires the requested resolution; at_most selects the best available resolution no higher than requested"},
				"filename":{"type":"string","description":"Optional output filename. The extension is normalized to the selected container."}
			},
			"required":["url","resolution"]
		}`),
	), env.download)
}

func (e YouTubeEnv) download(ctx context.Context, raw json.RawMessage) Result {
	var in struct {
		URL        string `json:"url"`
		Resolution int    `json:"resolution"`
		Selection  string `json:"selection"`
		Filename   string `json:"filename"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	videoURL, err := validateYouTubeURL(in.URL)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if in.Resolution < 144 || in.Resolution > 4320 {
		return Result{OK: false, Error: "resolution must be between 144 and 4320"}
	}
	mode := strings.ToLower(strings.TrimSpace(in.Selection))
	if mode == "" {
		mode = "exact"
	}
	if mode != "exact" && mode != "at_most" {
		return Result{OK: false, Error: "selection must be exact or at_most"}
	}
	if strings.TrimSpace(e.Workspace) == "" {
		return Result{OK: false, Error: "YouTube download workspace is not configured"}
	}

	timeout := e.Timeout
	if timeout <= 0 {
		timeout = defaultYouTubeTimeout
	}
	downloadCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ReportProgress(downloadCtx, Progress{Phase: "Reading video details", Percent: 1})
	client := e.Client
	if client == nil {
		client = &youtube.Client{HTTPClient: &http.Client{}}
	}
	video, err := client.GetVideoContext(downloadCtx, videoURL)
	if err != nil {
		return Result{OK: false, Error: "failed to read YouTube video metadata: " + err.Error()}
	}
	format, actualResolution, available, err := selectYouTubeFormat(video.Formats, in.Resolution, mode)
	if err != nil {
		return Result{OK: false, Error: err.Error(), Output: map[string]any{
			"requested_resolution":  in.Resolution,
			"available_resolutions": available,
		}}
	}
	ReportProgress(downloadCtx, Progress{Phase: "Preparing download", Percent: 3})

	maxBytes := e.maxBytes()
	mediaDir := filepath.Join(e.Workspace, "media")
	scratchDir := filepath.Join(e.Workspace, ".scratch")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if err := os.MkdirAll(scratchDir, 0o700); err != nil {
		return Result{OK: false, Error: err.Error()}
	}

	container := formatContainer(*format)
	name := youtubeOutputName(in.Filename, video.Title, container)
	dst, err := reserveMediaPath(mediaDir, name)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	success := false
	defer func() {
		if !success {
			_ = os.Remove(dst)
		}
	}()
	finish := func(downloadedBytes int64, downloader string, fallbackReason error) Result {
		info, statErr := os.Stat(dst)
		if statErr != nil {
			return Result{OK: false, Error: statErr.Error()}
		}
		if info.Size() > maxBytes {
			return Result{OK: false, Error: fmt.Sprintf("YouTube download exceeded the %d-byte limit", maxBytes)}
		}
		success = true
		rel, _ := filepath.Rel(e.Workspace, dst)
		rel = filepath.ToSlash(rel)
		if e.OnMutation != nil {
			e.OnMutation()
		}
		if e.OnApplied != nil {
			e.OnApplied(rel)
		}
		ReportProgress(downloadCtx, Progress{Phase: "Download complete", Current: info.Size(), Total: info.Size(), Percent: 100})
		out := map[string]any{
			"path":                 rel,
			"bytes":                info.Size(),
			"downloaded_bytes":     downloadedBytes,
			"title":                video.Title,
			"author":               video.Author,
			"duration_seconds":     video.Duration.Seconds(),
			"requested_resolution": in.Resolution,
			"resolution":           actualResolution,
			"width":                format.Width,
			"height":               format.Height,
			"fps":                  format.FPS,
			"itag":                 format.ItagNo,
			"mime_type":            format.MimeType,
			"has_audio":            true,
			"downloader":           downloader,
			"indexing":             "queued",
			"note":                 "The YouTube video is in the project media bin and is being transcribed and scene-indexed. Call place_media to put it on the timeline.",
		}
		if fallbackReason != nil {
			out["fallback_reason"] = fallbackReason.Error()
		}
		if media, probeErr := ffmpeg.ProbeMedia(ctx, e.Bins, e.Workspace, rel); probeErr == nil {
			out["duration_seconds"] = media.Duration
			out["width"] = media.Width
			out["height"] = media.Height
			out["has_audio"] = media.HasAudio
		}
		return Result{OK: true, Output: out}
	}
	fallback := func(primaryErr error) Result {
		ReportProgress(downloadCtx, Progress{Phase: "Switching downloader", Percent: 4})
		downloadedBytes, fallbackErr := e.downloadWithYTDLP(downloadCtx, videoURL, in.Resolution, mode, scratchDir, dst, maxBytes)
		if fallbackErr != nil {
			return Result{OK: false, Error: fmt.Sprintf("Go YouTube downloader failed: %v; yt-dlp fallback failed: %v", primaryErr, fallbackErr)}
		}
		return finish(downloadedBytes, "yt-dlp", primaryErr)
	}

	remaining := maxBytes
	hasAudio := format.AudioChannels > 0
	videoEnd := 75.0
	if hasAudio {
		videoEnd = 94
	}
	videoTmp, videoBytes, err := e.downloadStream(downloadCtx, client, video, format, scratchDir, ".video-*", &remaining, "Downloading video", 5, videoEnd)
	if err != nil {
		return fallback(err)
	}
	defer os.Remove(videoTmp)

	totalBytes := videoBytes
	if hasAudio {
		if err := replaceReservedFile(videoTmp, dst); err != nil {
			return Result{OK: false, Error: "failed to save downloaded video: " + err.Error()}
		}
	} else {
		audio := selectYouTubeAudio(video.Formats, container)
		if audio == nil {
			return fallback(errors.New("the selected video stream has no audio and no compatible audio stream is available"))
		}
		audioTmp, audioBytes, downloadErr := e.downloadStream(downloadCtx, client, video, audio, scratchDir, ".audio-*", &remaining, "Downloading audio", 75, 91)
		if downloadErr != nil {
			return fallback(downloadErr)
		}
		defer os.Remove(audioTmp)
		totalBytes += audioBytes
		ReportProgress(downloadCtx, Progress{Phase: "Combining video and audio", Current: totalBytes, Percent: 94})
		if err := e.mergeStreams(downloadCtx, videoTmp, audioTmp, dst, container, audio.MimeType, timeout); err != nil {
			return fallback(err)
		}
		hasAudio = true
	}

	return finish(totalBytes, "kkdai/youtube", nil)
}

func (e YouTubeEnv) downloadWithYTDLP(ctx context.Context, videoURL string, resolution int, mode, scratchDir, destination string, maxBytes int64) (int64, error) {
	bin := strings.TrimSpace(e.YTDLPBin)
	if bin == "" {
		bin = "yt-dlp"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return 0, fmt.Errorf("%s is not installed: %w", bin, err)
	}
	marker, err := os.CreateTemp(scratchDir, ".ytdlp-*")
	if err != nil {
		return 0, err
	}
	base := marker.Name()
	if err := marker.Close(); err != nil {
		return 0, err
	}
	_ = os.Remove(base)
	defer func() {
		matches, _ := filepath.Glob(base + "*")
		for _, match := range matches {
			_ = os.Remove(match)
		}
	}()

	comparison := "="
	if mode == "at_most" {
		comparison = "<="
	}
	filter := fmt.Sprintf("height%s%d", comparison, resolution)
	// Prefer AVC in MP4 when YouTube offers it at the requested height. That
	// codec plays directly in browsers and avoids an unnecessary AV1 proxy.
	selector := fmt.Sprintf("bestvideo[%s][vcodec^=avc1][ext=mp4]+bestaudio[ext=m4a]/bestvideo[%s][ext=mp4]+bestaudio[ext=m4a]/bestvideo[%s]+bestaudio/best[%s]", filter, filter, filter, filter)
	args := []string{
		"--no-playlist",
		"--newline",
		"--progress-delta", "0.25",
		"--progress-template", "download:parallax-progress:%(progress.downloaded_bytes)s:%(progress.total_bytes_estimate)s:%(progress.total_bytes)s:%(progress._percent_str)s",
		"--no-part",
		"--force-overwrites",
		"--force-ipv4",
		"--max-filesize", strconv.FormatInt(maxBytes, 10),
		"--format", selector,
		"--merge-output-format", "mp4",
		"--remux-video", "mp4",
		"--output", base + ".%(ext)s",
		"--print", "after_move:filepath",
	}
	if ffmpegBin := strings.TrimSpace(e.Bins.FFmpeg); ffmpegBin != "" && ffmpegBin != "ffmpeg" {
		args = append(args, "--ffmpeg-location", ffmpegBin)
	}
	args = append(args, videoURL)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = e.Workspace
	var stdout bytes.Buffer
	lastPercent := 4.0
	stderr := &youtubeProgressCapture{onLine: func(line string) {
		current, total, percent, ok := parseYTDLPProgress(line)
		if !ok {
			return
		}
		mapped := 5 + percent*.88
		if mapped < lastPercent+.2 && mapped < 93 {
			return
		}
		if mapped > 93 {
			mapped = 93
		}
		lastPercent = mapped
		ReportProgress(ctx, Progress{Phase: "Downloading video", Current: current, Total: total, Percent: mapped})
	}}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return 0, errors.New(trimOutput(message, 8<<10))
	}
	outputPath := lastNonEmptyLine(stdout.String())
	if outputPath == "" {
		return 0, errors.New("yt-dlp did not report an output file")
	}
	if !filepath.IsAbs(outputPath) {
		outputPath = filepath.Join(e.Workspace, outputPath)
	}
	outputPath = filepath.Clean(outputPath)
	rel, err := filepath.Rel(scratchDir, outputPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return 0, errors.New("yt-dlp output escaped the project scratch directory")
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		return 0, fmt.Errorf("yt-dlp output is missing: %w", err)
	}
	if info.Size() > maxBytes {
		return 0, fmt.Errorf("YouTube download exceeded the %d-byte limit", maxBytes)
	}
	ReportProgress(ctx, Progress{Phase: "Processing download", Current: info.Size(), Total: info.Size(), Percent: 96})
	if err := replaceReservedFile(outputPath, destination); err != nil {
		return 0, fmt.Errorf("failed to import yt-dlp output: %w", err)
	}
	return info.Size(), nil
}

func lastNonEmptyLine(value string) string {
	lines := strings.Split(strings.TrimSpace(value), "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		if line := strings.TrimSpace(lines[index]); line != "" {
			return line
		}
	}
	return ""
}

func (e YouTubeEnv) downloadStream(ctx context.Context, client YouTubeClient, video *youtube.Video, format *youtube.Format, dir, pattern string, remaining *int64, phase string, startPercent, endPercent float64) (string, int64, error) {
	stream, declared, err := client.GetStreamContext(ctx, video, format)
	if err != nil {
		return "", 0, fmt.Errorf("failed to open YouTube stream: %w", err)
	}
	defer stream.Close()
	if declared > *remaining {
		return "", 0, fmt.Errorf("YouTube stream is too large: %d bytes exceeds the remaining %d-byte limit", declared, *remaining)
	}
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", 0, err
	}
	path := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	written, err := copyYouTubeStream(ctx, tmp, io.LimitReader(stream, *remaining+1), declared, phase, startPercent, endPercent)
	if err != nil {
		return "", 0, fmt.Errorf("failed to download YouTube stream: %w", err)
	}
	if written > *remaining {
		return "", 0, fmt.Errorf("YouTube download exceeded the %d-byte limit", e.maxBytes())
	}
	if err := tmp.Sync(); err != nil {
		return "", 0, err
	}
	if err := tmp.Close(); err != nil {
		return "", 0, err
	}
	*remaining -= written
	ok = true
	return path, written, nil
}

func copyYouTubeStream(ctx context.Context, dst io.Writer, src io.Reader, total int64, phase string, startPercent, endPercent float64) (int64, error) {
	buffer := make([]byte, 256<<10)
	var written int64
	lastPercent := startPercent - 1
	ReportProgress(ctx, Progress{Phase: phase, Total: total, Percent: startPercent})
	for {
		n, readErr := src.Read(buffer)
		if n > 0 {
			wn, writeErr := dst.Write(buffer[:n])
			written += int64(wn)
			if writeErr != nil {
				return written, writeErr
			}
			if wn != n {
				return written, io.ErrShortWrite
			}
			percent := startPercent
			if total > 0 {
				percent += float64(written) / float64(total) * (endPercent - startPercent)
			}
			if percent >= lastPercent+0.5 || written == total {
				lastPercent = percent
				ReportProgress(ctx, Progress{Phase: phase, Current: written, Total: total, Percent: min(percent, endPercent)})
			}
		}
		if readErr == io.EOF {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}

type youtubeProgressCapture struct {
	log     bytes.Buffer
	pending string
	onLine  func(string)
}

func (c *youtubeProgressCapture) Write(p []byte) (int, error) {
	_, _ = c.log.Write(p)
	c.pending += string(p)
	for {
		index := strings.IndexAny(c.pending, "\r\n")
		if index < 0 {
			break
		}
		line := strings.TrimSpace(c.pending[:index])
		c.pending = strings.TrimLeft(c.pending[index+1:], "\r\n")
		if line != "" && c.onLine != nil {
			c.onLine(line)
		}
	}
	return len(p), nil
}

func (c *youtubeProgressCapture) String() string { return c.log.String() }

func parseYTDLPProgress(line string) (int64, int64, float64, bool) {
	const prefix = "parallax-progress:"
	line = strings.TrimSpace(line)
	index := strings.Index(line, prefix)
	if index < 0 {
		return 0, 0, 0, false
	}
	parts := strings.Split(strings.TrimSpace(line[index+len(prefix):]), ":")
	if len(parts) < 4 {
		return 0, 0, 0, false
	}
	current, _ := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	total, _ := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if total <= 0 {
		total, _ = strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64)
	}
	percentText := strings.TrimSpace(strings.TrimSuffix(parts[3], "%"))
	percent, err := strconv.ParseFloat(percentText, 64)
	if err != nil && total > 0 {
		percent = float64(current) / float64(total) * 100
		err = nil
	}
	if err != nil {
		return current, total, 0, false
	}
	return current, total, min(max(percent, 0), 100), true
}

func (e YouTubeEnv) maxBytes() int64 {
	if e.MaxBytes > 0 {
		return e.MaxBytes
	}
	return defaultYouTubeMaxBytes
}

func (e YouTubeEnv) mergeStreams(ctx context.Context, videoPath, audioPath, dst, container, audioMIME string, timeout time.Duration) error {
	videoRel, _ := filepath.Rel(e.Workspace, videoPath)
	audioRel, _ := filepath.Rel(e.Workspace, audioPath)
	dstRel, _ := filepath.Rel(e.Workspace, dst)
	args := []string{"ffmpeg", "-y", "-i", filepath.ToSlash(videoRel), "-i", filepath.ToSlash(audioRel), "-map", "0:v:0", "-map", "1:a:0", "-c:v", "copy"}
	if container == "webm" {
		if strings.Contains(audioMIME, "webm") || strings.Contains(audioMIME, "opus") {
			args = append(args, "-c:a", "copy")
		} else {
			args = append(args, "-c:a", "libopus")
		}
	} else {
		args = append(args, "-c:a", "aac", "-movflags", "+faststart")
	}
	args = append(args, filepath.ToSlash(dstRel))
	cmd, err := ffmpeg.Validate(args, ffmpeg.ValidateOpts{Workspace: e.Workspace})
	if err != nil {
		return fmt.Errorf("failed to validate YouTube merge: %w", err)
	}
	if _, err := ffmpeg.Run(ctx, e.Bins, cmd, e.Workspace, timeout); err != nil {
		return fmt.Errorf("failed to merge YouTube video and audio: %w", err)
	}
	return nil
}

func validateYouTubeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return "", errors.New("url must be a valid HTTPS YouTube video URL")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host != "youtu.be" && host != "youtube.com" && !strings.HasSuffix(host, ".youtube.com") && host != "youtube-nocookie.com" && !strings.HasSuffix(host, ".youtube-nocookie.com") {
		return "", errors.New("url host must be youtube.com or youtu.be")
	}
	return u.String(), nil
}

func selectYouTubeFormat(formats youtube.FormatList, requested int, mode string) (*youtube.Format, int, []int, error) {
	availableSet := map[int]bool{}
	for _, format := range formats {
		if strings.HasPrefix(format.MimeType, "video/") {
			if resolution := youtubeResolution(format); resolution > 0 {
				availableSet[resolution] = true
			}
		}
	}
	available := make([]int, 0, len(availableSet))
	for resolution := range availableSet {
		available = append(available, resolution)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(available)))
	selectedResolution := requested
	if !availableSet[selectedResolution] && mode == "at_most" {
		selectedResolution = 0
		for _, resolution := range available {
			if resolution <= requested {
				selectedResolution = resolution
				break
			}
		}
	}
	if selectedResolution == 0 || !availableSet[selectedResolution] {
		return nil, 0, available, fmt.Errorf("resolution %dp is not available in %s mode", requested, mode)
	}
	candidates := make([]youtube.Format, 0)
	for _, format := range formats {
		if strings.HasPrefix(format.MimeType, "video/") && youtubeResolution(format) == selectedResolution {
			candidates = append(candidates, format)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		iMP4 := strings.HasPrefix(candidates[i].MimeType, "video/mp4")
		jMP4 := strings.HasPrefix(candidates[j].MimeType, "video/mp4")
		if iMP4 != jMP4 {
			return iMP4
		}
		iAVC := strings.Contains(strings.ToLower(candidates[i].MimeType), "avc1")
		jAVC := strings.Contains(strings.ToLower(candidates[j].MimeType), "avc1")
		if iAVC != jAVC {
			return iAVC
		}
		if candidates[i].FPS != candidates[j].FPS {
			return candidates[i].FPS > candidates[j].FPS
		}
		return candidates[i].Bitrate > candidates[j].Bitrate
	})
	if len(candidates) == 0 {
		return nil, 0, available, fmt.Errorf("no downloadable video format is available at %dp", selectedResolution)
	}
	return &candidates[0], selectedResolution, available, nil
}

func selectYouTubeAudio(formats youtube.FormatList, container string) *youtube.Format {
	candidates := make([]youtube.Format, 0)
	for _, format := range formats {
		if strings.HasPrefix(format.MimeType, "audio/") && format.AudioChannels > 0 {
			candidates = append(candidates, format)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		iMatch := strings.Contains(candidates[i].MimeType, container)
		jMatch := strings.Contains(candidates[j].MimeType, container)
		if iMatch != jMatch {
			return iMatch
		}
		return candidates[i].Bitrate > candidates[j].Bitrate
	})
	if len(candidates) == 0 {
		return nil
	}
	return &candidates[0]
}

func youtubeResolution(format youtube.Format) int {
	label := strings.ToLower(strings.TrimSpace(format.QualityLabel))
	if index := strings.IndexByte(label, 'p'); index > 0 {
		if value, err := strconv.Atoi(label[:index]); err == nil {
			return value
		}
	}
	return format.Height
}

func formatContainer(format youtube.Format) string {
	if strings.HasPrefix(format.MimeType, "video/webm") {
		return "webm"
	}
	return "mp4"
}

func youtubeOutputName(requested, title, container string) string {
	name := sanitizeImageName(requested)
	if name == "" {
		name = sanitizeImageName(title)
	}
	if name == "" {
		name = "youtube-video"
	}
	return strings.TrimSuffix(name, filepath.Ext(name)) + "." + container
}

func reserveMediaPath(dir, name string) (string, error) {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for index := 0; index < 10000; index++ {
		candidate := filepath.Join(dir, name)
		if index > 0 {
			candidate = filepath.Join(dir, fmt.Sprintf("%s-%d%s", base, index, ext))
		}
		file, err := os.OpenFile(candidate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if err := file.Close(); err != nil {
			_ = os.Remove(candidate)
			return "", err
		}
		return candidate, nil
	}
	return "", errors.New("could not allocate a media filename")
}

func replaceReservedFile(source, destination string) error {
	if err := os.Rename(source, destination); err == nil {
		return nil
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
