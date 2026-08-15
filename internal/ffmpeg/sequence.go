package ffmpeg

import (
	"fmt"
	"strconv"
	"strings"
)

// SequenceClip is one timeline item in seconds, using project-relative paths.
type SequenceClip struct {
	Track     string
	Kind      string
	Path      string
	Name      string
	MediaType string
	Start     float64
	Duration  float64
	SourceIn  float64
}

// BuildSequenceArgs renders the timeline the same way Program plays it:
// black gaps, V1 picture, V2 titles over V1, mixed A1/A2.
func BuildSequenceArgs(spec ExportSpec, clips []SequenceClip, dest string) ([]string, error) {
	if err := spec.Normalize(); err != nil {
		return nil, err
	}
	dest = strings.TrimSpace(dest)
	if dest == "" {
		return nil, fmt.Errorf("export destination is required")
	}
	if len(clips) == 0 {
		return nil, fmt.Errorf("sequence is empty")
	}

	seqDur := sequenceEnd(clips)
	if spec.Start >= seqDur {
		return nil, fmt.Errorf("start is past the end of the sequence")
	}
	outDur := seqDur
	if spec.Duration > 0 {
		outDur = spec.Duration
	}
	if spec.Start+outDur > seqDur {
		outDur = seqDur - spec.Start
	}
	if outDur <= 0 {
		return nil, fmt.Errorf("sequence has no duration")
	}

	w, h := 1920, 1080
	if cw, ch, ok := parseSize(spec.Resolution); ok {
		w, h = cw, ch
	}
	fps := spec.FPS
	if fps <= 0 {
		fps = 24
	}
	if spec.Format == "gif" && spec.FPS == 0 {
		fps = 12
	}

	pictures := pictureClips(clips)
	audios := audioClips(clips)
	titles := titleClips(clips)
	wantAudio := spec.Audio && spec.Format != "gif"
	if spec.Format == "mp3" {
		wantAudio = true
		if len(audios) == 0 {
			return nil, fmt.Errorf("sequence has no audio to export")
		}
	}

	args := []string{"-y", "-hide_banner"}
	inputOf := map[int]int{}
	next := 0

	if spec.Format != "mp3" {
		args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("color=c=black:s=%dx%d:d=%s:r=%d", w, h, formatSeconds(seqDur), fps))
		next++
	}
	silence := -1
	if wantAudio {
		args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("anullsrc=r=48000:cl=stereo:d=%s", formatSeconds(seqDur)))
		silence = next
		next++
	}

	for i, clip := range pictures {
		if clip.MediaType == "image" {
			args = append(args, "-loop", "1", "-framerate", strconv.Itoa(fps), "-t", formatSeconds(clip.Duration), "-i", clip.Path)
		} else {
			if clip.SourceIn > 0 {
				args = append(args, "-ss", formatSeconds(clip.SourceIn))
			}
			args = append(args, "-t", formatSeconds(clip.Duration), "-i", clip.Path)
		}
		inputOf[i] = next
		next++
	}
	audioOf := map[int]int{}
	if wantAudio {
		for i, clip := range audios {
			if clip.SourceIn > 0 {
				args = append(args, "-ss", formatSeconds(clip.SourceIn))
			}
			args = append(args, "-t", formatSeconds(clip.Duration), "-i", clip.Path)
			audioOf[i] = next
			next++
		}
	}

	var filters []string
	videoOut := ""
	if spec.Format != "mp3" {
		cur := "0:v"
		for i, clip := range pictures {
			in := inputOf[i]
			label := fmt.Sprintf("v%d", i)
			scaled := fmt.Sprintf("[%d:v]scale=%d:%d:force_original_aspect_ratio=decrease:force_divisible_by=2,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setpts=PTS-STARTPTS+%s/TB[%s]",
				in, w, h, w, h, formatSeconds(clip.Start), label)
			filters = append(filters, scaled)
			out := fmt.Sprintf("ov%d", i)
			filters = append(filters, fmt.Sprintf("[%s][%s]overlay=0:0:eof_action=pass:enable='between(t,%s,%s)'[%s]",
				cur, label, formatSeconds(clip.Start), formatSeconds(clip.Start+clip.Duration), out))
			cur = out
		}
		for i, clip := range titles {
			out := fmt.Sprintf("t%d", i)
			text := escapeDrawText(clip.Name)
			filters = append(filters, fmt.Sprintf("[%s]drawtext=text='%s':fontcolor=white:fontsize=h/16:x=(w-text_w)/2:y=h-h/8:enable='between(t,%s,%s)'[%s]",
				cur, text, formatSeconds(clip.Start), formatSeconds(clip.Start+clip.Duration), out))
			cur = out
		}
		if !strings.Contains(cur, ":") {
			videoOut = cur
		} else {
			filters = append(filters, fmt.Sprintf("[%s]null[vout]", cur))
			videoOut = "vout"
		}
	}

	audioOut := ""
	if wantAudio {
		mix := []string{fmt.Sprintf("[%d:a]", silence)}
		for i, clip := range audios {
			in := audioOf[i]
			label := fmt.Sprintf("a%d", i)
			delay := int(clip.Start*1000 + 0.5)
			filters = append(filters, fmt.Sprintf("[%d:a]aresample=48000,aformat=sample_fmts=fltp:channel_layouts=stereo,adelay=%d:all=1[%s]",
				in, delay, label))
			mix = append(mix, "["+label+"]")
		}
		if len(mix) == 1 {
			audioOut = fmt.Sprintf("%d:a", silence)
		} else {
			filters = append(filters, fmt.Sprintf("%samix=inputs=%d:duration=first:dropout_transition=0[aout]", strings.Join(mix, ""), len(mix)))
			audioOut = "aout"
		}
	}

	if len(filters) > 0 {
		args = append(args, "-filter_complex", strings.Join(filters, ";"))
	}
	if videoOut != "" {
		args = append(args, "-map", "["+videoOut+"]")
	}
	if audioOut != "" {
		if strings.Contains(audioOut, ":") {
			args = append(args, "-map", audioOut)
		} else {
			args = append(args, "-map", "["+audioOut+"]")
		}
	}

	if spec.Start > 0 {
		args = append(args, "-ss", formatSeconds(spec.Start))
	}
	args = append(args, "-t", formatSeconds(outDur))

	if spec.Format == "mp3" {
		args = append(args, "-c:a", "libmp3lame", "-q:a", mp3Quality(spec.Quality), dest)
		return args, nil
	}
	if spec.Format == "gif" {
		args = append(args, "-an", "-loop", "0", dest)
		return args, nil
	}

	switch spec.Format {
	case "webm":
		args = append(args, "-c:v", "libvpx-vp9", "-b:v", "0", "-crf", vp9CRF(spec.Quality), "-pix_fmt", "yuv420p")
		if wantAudio {
			args = append(args, "-c:a", "libopus", "-b:a", "128k")
		} else {
			args = append(args, "-an")
		}
	default:
		preset, crf := x264Quality(spec.Quality)
		args = append(args, "-c:v", "libx264", "-preset", preset, "-crf", crf, "-pix_fmt", "yuv420p")
		if wantAudio {
			args = append(args, "-c:a", "aac", "-b:a", "192k")
		} else {
			args = append(args, "-an")
		}
	}
	args = append(args, dest)
	return args, nil
}

func sequenceEnd(clips []SequenceClip) float64 {
	var end float64
	for _, clip := range clips {
		if next := clip.Start + clip.Duration; next > end {
			end = next
		}
	}
	return end
}

func pictureClips(clips []SequenceClip) []SequenceClip {
	var out []SequenceClip
	for _, clip := range clips {
		if clip.Kind != "video" || clip.Path == "" {
			continue
		}
		out = append(out, clip)
	}
	return out
}

func audioClips(clips []SequenceClip) []SequenceClip {
	var out []SequenceClip
	for _, clip := range clips {
		if clip.Kind != "audio" || clip.Path == "" {
			continue
		}
		out = append(out, clip)
	}
	return out
}

func titleClips(clips []SequenceClip) []SequenceClip {
	var out []SequenceClip
	for _, clip := range clips {
		if clip.Kind == "title" {
			out = append(out, clip)
		}
	}
	return out
}

func escapeDrawText(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	s = strings.ReplaceAll(s, `:`, `\:`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	return s
}
