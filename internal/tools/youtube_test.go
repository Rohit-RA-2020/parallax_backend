package tools

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kkdai/youtube/v2"
)

type fakeYouTubeClient struct {
	video     *youtube.Video
	streams   map[int][]byte
	seenURL   string
	streamErr error
}

func (f *fakeYouTubeClient) GetVideoContext(_ context.Context, rawURL string) (*youtube.Video, error) {
	f.seenURL = rawURL
	return f.video, nil
}

func (f *fakeYouTubeClient) GetStreamContext(_ context.Context, _ *youtube.Video, format *youtube.Format) (io.ReadCloser, int64, error) {
	if f.streamErr != nil {
		return nil, 0, f.streamErr
	}
	data := f.streams[format.ItagNo]
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

func TestDownloadYouTubeFallsBackToYTDLP(t *testing.T) {
	workspace := t.TempDir()
	downloader := filepath.Join(t.TempDir(), "yt-dlp")
	script := `#!/bin/sh
output=''
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--output" ]; then
    shift
    output="$1"
  fi
  shift
done
path=$(printf '%s' "$output" | sed 's/%(ext)s/mp4/g')
printf 'parallax-progress:8:16:NA: 50.0%%\n' >&2
printf 'fallback-video' > "$path"
printf '%s\n' "$path"
`
	if err := os.WriteFile(downloader, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	client := &fakeYouTubeClient{
		video: &youtube.Video{Title: "Fallback", Formats: youtube.FormatList{
			{ItagNo: 137, MimeType: "video/mp4", QualityLabel: "1080p", Width: 1920, Height: 1080},
		}},
		streamErr: errors.New("unexpected status code: 403"),
	}
	reg := NewRegistry()
	RegisterYouTube(reg, YouTubeEnv{Workspace: workspace, Client: client, YTDLPBin: downloader, MaxBytes: 1024})
	var progress []Progress
	ctx := WithProgress(context.Background(), func(item Progress) { progress = append(progress, item) })
	res := reg.Execute(ctx, "download_youtube_video", `{"url":"https://youtu.be/abc123","resolution":1080,"selection":"at_most"}`)
	if !res.OK {
		t.Fatal(res.Error)
	}
	out := res.Output.(map[string]any)
	if out["downloader"] != "yt-dlp" || !strings.Contains(out["fallback_reason"].(string), "403") {
		t.Fatalf("output=%#v", out)
	}
	path := filepath.Join(workspace, filepath.FromSlash(out["path"].(string)))
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "fallback-video" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	if !hasProgressPhase(progress, "Downloading video") || progress[len(progress)-1].Percent != 100 {
		t.Fatalf("progress=%+v", progress)
	}
}

func hasProgressPhase(items []Progress, phase string) bool {
	for _, item := range items {
		if item.Phase == phase {
			return true
		}
	}
	return false
}

func TestDownloadYouTubeMuxedExactResolution(t *testing.T) {
	workspace := t.TempDir()
	client := &fakeYouTubeClient{
		video: &youtube.Video{
			Title: "A Useful / Video", Author: "Creator", Duration: 2 * time.Minute,
			Formats: youtube.FormatList{
				{ItagNo: 18, MimeType: `video/mp4; codecs="avc1, mp4a"`, QualityLabel: "360p", Width: 640, Height: 360, FPS: 30, Bitrate: 500, AudioChannels: 2},
				{ItagNo: 22, MimeType: `video/mp4; codecs="avc1, mp4a"`, QualityLabel: "720p", Width: 1280, Height: 720, FPS: 30, Bitrate: 900, AudioChannels: 2},
			},
		},
		streams: map[int][]byte{22: []byte("muxed-video")},
	}
	mutated := false
	var applied string
	reg := NewRegistry()
	RegisterYouTube(reg, YouTubeEnv{
		Workspace: workspace, Client: client, MaxBytes: 1024,
		OnMutation: func() { mutated = true }, OnApplied: func(rel string) { applied = rel },
	})
	var progress []Progress
	ctx := WithProgress(context.Background(), func(item Progress) { progress = append(progress, item) })
	res := reg.Execute(ctx, "download_youtube_video", `{"url":"https://youtu.be/abc123","resolution":720,"filename":"my clip.mov"}`)
	if !res.OK {
		t.Fatal(res.Error)
	}
	if client.seenURL != "https://youtu.be/abc123" {
		t.Fatalf("url=%q", client.seenURL)
	}
	out := res.Output.(map[string]any)
	if out["resolution"] != 720 || out["path"] != "media/my-clip.mp4" {
		t.Fatalf("output=%#v", out)
	}
	if !mutated || applied != "media/my-clip.mp4" {
		t.Fatalf("callbacks mutated=%v applied=%q", mutated, applied)
	}
	if len(progress) < 2 || progress[len(progress)-1].Percent != 100 || progress[len(progress)-1].Phase != "Download complete" {
		t.Fatalf("progress=%+v", progress)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "media", "my-clip.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "muxed-video" {
		t.Fatalf("data=%q", data)
	}
}

func TestParseYTDLPProgress(t *testing.T) {
	current, total, percent, ok := parseYTDLPProgress("parallax-progress:524288:1048576:NA: 50.0%")
	if !ok || current != 524288 || total != 1048576 || percent != 50 {
		t.Fatalf("current=%d total=%d percent=%v ok=%v", current, total, percent, ok)
	}
}

func TestSelectYouTubeFormatExactAndAtMost(t *testing.T) {
	formats := youtube.FormatList{
		{ItagNo: 1, MimeType: "video/webm", QualityLabel: "1080p", FPS: 60, Bitrate: 2000},
		{ItagNo: 4, MimeType: `video/mp4; codecs="av01"`, QualityLabel: "1080p", FPS: 60, Bitrate: 2500},
		{ItagNo: 2, MimeType: `video/mp4; codecs="avc1"`, QualityLabel: "1080p", FPS: 30, Bitrate: 1500},
		{ItagNo: 3, MimeType: "video/mp4", QualityLabel: "720p", FPS: 60, Bitrate: 1000},
	}
	format, resolution, available, err := selectYouTubeFormat(formats, 1080, "exact")
	if err != nil {
		t.Fatal(err)
	}
	if format.ItagNo != 2 || resolution != 1080 {
		t.Fatalf("format=%+v resolution=%d", format, resolution)
	}
	if !reflect.DeepEqual(available, []int{1080, 720}) {
		t.Fatalf("available=%v", available)
	}
	format, resolution, _, err = selectYouTubeFormat(formats, 900, "at_most")
	if err != nil || format.ItagNo != 3 || resolution != 720 {
		t.Fatalf("format=%+v resolution=%d err=%v", format, resolution, err)
	}
	_, _, available, err = selectYouTubeFormat(formats, 900, "exact")
	if err == nil || !strings.Contains(err.Error(), "900p") || !reflect.DeepEqual(available, []int{1080, 720}) {
		t.Fatalf("available=%v err=%v", available, err)
	}
}

func TestDownloadYouTubeRejectsUnsafeURLAndOversizeStream(t *testing.T) {
	reg := NewRegistry()
	RegisterYouTube(reg, YouTubeEnv{Workspace: t.TempDir()})
	res := reg.Execute(context.Background(), "download_youtube_video", `{"url":"https://youtube.com.evil.example/watch?v=x","resolution":720}`)
	if res.OK || !strings.Contains(res.Error, "host") {
		t.Fatalf("result=%+v", res)
	}

	client := &fakeYouTubeClient{
		video: &youtube.Video{Title: "large", Formats: youtube.FormatList{
			{ItagNo: 22, MimeType: "video/mp4", QualityLabel: "720p", AudioChannels: 2},
		}},
		streams: map[int][]byte{22: bytes.Repeat([]byte("x"), 33)},
	}
	reg = NewRegistry()
	RegisterYouTube(reg, YouTubeEnv{Workspace: t.TempDir(), Client: client, MaxBytes: 32})
	res = reg.Execute(context.Background(), "download_youtube_video", `{"url":"https://www.youtube.com/watch?v=x","resolution":720}`)
	if res.OK || !strings.Contains(res.Error, "too large") {
		t.Fatalf("result=%+v", res)
	}
}
