package ffmpeg

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// SequenceClip is one timeline item in seconds, using project-relative paths.
type SequenceClip struct {
	Track        string
	Kind         string
	Path         string
	Name         string
	MediaType    string
	Start        float64
	Duration     float64
	SourceIn     float64
	CanvasWidth  int
	CanvasHeight int
	TitleText    string
	FontSize     float64
	Fill         string
	X            float64
	Y            float64
	AnchorX      float64
	AnchorY      float64
	Opacity      float64
	OpacityKeys  []SequenceKeyframe
	PlaybackRate float64
	VolumeDB     float64
	Muted        bool
	ScaleX       float64
	ScaleY       float64
	Rotation     float64
	CropTop      float64
	CropRight    float64
	CropBottom   float64
	CropLeft     float64
	Exposure     float64
	Contrast     float64
	Saturation   float64
	FadeIn       float64
	FadeOut      float64
	FadeColor    string
	CrossfadeIn  bool
	SubtitlePath string
	CaptionLang  string
	FontName     string
	FontsDir     string
}

type SequenceKeyframe struct {
	Frame  int
	Value  float64
	Easing string
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
	captions := captionClips(clips)
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
		inputDuration := clip.Duration
		if clip.PlaybackRate > 0 {
			inputDuration *= clip.PlaybackRate
		}
		if clip.MediaType == "image" {
			args = append(args, "-loop", "1", "-framerate", strconv.Itoa(fps), "-t", formatSeconds(clip.Duration), "-i", clip.Path)
		} else {
			if clip.SourceIn > 0 {
				args = append(args, "-ss", formatSeconds(clip.SourceIn))
			}
			args = append(args, "-t", formatSeconds(inputDuration), "-i", clip.Path)
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
			inputDuration := clip.Duration
			if clip.PlaybackRate > 0 {
				inputDuration *= clip.PlaybackRate
			}
			args = append(args, "-t", formatSeconds(inputDuration), "-i", clip.Path)
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
			rate := clip.PlaybackRate
			if rate <= 0 {
				rate = 1
			}
			chain := []string{}
			if clip.CropTop != 0 || clip.CropRight != 0 || clip.CropBottom != 0 || clip.CropLeft != 0 {
				chain = append(chain, fmt.Sprintf("crop=iw*(1-%s-%s):ih*(1-%s-%s):iw*%s:ih*%s", formatSeconds(clip.CropLeft), formatSeconds(clip.CropRight), formatSeconds(clip.CropTop), formatSeconds(clip.CropBottom), formatSeconds(clip.CropLeft), formatSeconds(clip.CropTop)))
			}
			chain = append(chain, fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease:force_divisible_by=2", w, h), fmt.Sprintf("pad=%d:%d:(ow-iw)/2:(oh-ih)/2", w, h))
			if clip.Exposure != 0 || clip.Contrast != 0 || clip.Saturation != 0 {
				chain = append(chain, fmt.Sprintf("eq=brightness=%s:contrast=%s:saturation=%s", formatSeconds(clip.Exposure/5), formatSeconds(1+clip.Contrast), formatSeconds(1+clip.Saturation)))
			}
			sx, sy := clip.ScaleX, clip.ScaleY
			if sx == 0 {
				sx = 1
			}
			if sy == 0 {
				sy = 1
			}
			if sx != 1 || sy != 1 {
				chain = append(chain, fmt.Sprintf("scale=iw*%s:ih*%s", formatSeconds(sx), formatSeconds(sy)))
			}
			if clip.Rotation != 0 {
				chain = append(chain, fmt.Sprintf("rotate=%s*PI/180:ow=rotw(iw):oh=roth(ih):c=none", formatSeconds(clip.Rotation)))
			}
			if clip.Opacity > 0 && clip.Opacity < 1 {
				chain = append(chain, "format=rgba", "colorchannelmixer=aa="+formatSeconds(clip.Opacity))
			}
			if clip.CrossfadeIn && clip.FadeIn > 0 {
				chain = append(chain, "format=rgba", fmt.Sprintf("fade=t=in:st=0:d=%s:alpha=1", formatSeconds(clip.FadeIn)))
			} else if clip.FadeIn > 0 {
				color := clip.FadeColor
				if color == "" {
					color = "black"
				}
				chain = append(chain, fmt.Sprintf("fade=t=in:st=0:d=%s:color=%s", formatSeconds(clip.FadeIn), color))
			}
			if clip.FadeOut > 0 {
				color := clip.FadeColor
				if color == "" {
					color = "black"
				}
				start := max(0.0, clip.Duration-clip.FadeOut)
				chain = append(chain, fmt.Sprintf("fade=t=out:st=%s:d=%s:color=%s", formatSeconds(start), formatSeconds(clip.FadeOut), color))
			}
			chain = append(chain, fmt.Sprintf("setpts=(PTS-STARTPTS)/%s+%s/TB", formatSeconds(rate), formatSeconds(clip.Start)))
			scaled := fmt.Sprintf("[%d:v]%s[%s]", in, strings.Join(chain, ","), label)
			filters = append(filters, scaled)
			out := fmt.Sprintf("ov%d", i)
			cw, ch := clip.CanvasWidth, clip.CanvasHeight
			if cw <= 0 {
				cw = 1920
			}
			if ch <= 0 {
				ch = 1080
			}
			x, y := clip.X, clip.Y
			if x == 0 {
				x = float64(cw) / 2
			}
			if y == 0 {
				y = float64(ch) / 2
			}
			xexpr := fmt.Sprintf("(W-w)/2+(%s/%d-.5)*W", formatSeconds(x), cw)
			yexpr := fmt.Sprintf("(H-h)/2+(%s/%d-.5)*H", formatSeconds(y), ch)
			filters = append(filters, fmt.Sprintf("[%s][%s]overlay=x='%s':y='%s':eof_action=pass:enable='between(t,%s,%s)'[%s]", cur, label, xexpr, yexpr, formatSeconds(clip.Start), formatSeconds(clip.Start+clip.Duration), out))
			cur = out
		}
		for i, clip := range titles {
			out := fmt.Sprintf("t%d", i)
			text := clip.TitleText
			if text == "" {
				text = clip.Name
			}
			text = escapeDrawText(text)
			fill := strings.TrimPrefix(clip.Fill, "#")
			if fill == "" {
				fill = "ffffff"
			}
			fontSize := clip.FontSize
			if fontSize <= 0 {
				fontSize = 64
			}
			cw, ch := clip.CanvasWidth, clip.CanvasHeight
			if cw <= 0 {
				cw = 1920
			}
			if ch <= 0 {
				ch = 1080
			}
			x, y := clip.X, clip.Y
			if x == 0 {
				x = float64(cw) / 2
			}
			if y == 0 {
				y = 96
			}
			xexpr := fmt.Sprintf("%s*w/%d-%s*text_w", formatSeconds(x), cw, formatSeconds(clip.AnchorX))
			yexpr := fmt.Sprintf("%s*h/%d-%s*text_h", formatSeconds(y), ch, formatSeconds(clip.AnchorY))
			filters = append(filters, fmt.Sprintf("[%s]drawtext=text='%s':fontcolor=%s:fontsize=%s*h/%d:x=%s:y=%s:alpha='%s':enable='between(t,%s,%s)'[%s]",
				cur, text, fill, formatSeconds(fontSize), ch, xexpr, yexpr, opacityExpression(clip, fps), formatSeconds(clip.Start), formatSeconds(clip.Start+clip.Duration), out))
			cur = out
		}
		for i, clip := range captions {
			if strings.TrimSpace(clip.SubtitlePath) == "" {
				continue
			}
			out := fmt.Sprintf("cap%d", i)
			filter := subtitleFilter(clip.SubtitlePath, CaptionFont{Name: clip.FontName, FontsDir: clip.FontsDir})
			filters = append(filters, fmt.Sprintf("[%s]%s[%s]", cur, filter, out))
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
			parts := []string{"aresample=48000", "aformat=sample_fmts=fltp:channel_layouts=stereo"}
			if clip.PlaybackRate > 0 && clip.PlaybackRate != 1 {
				parts = append(parts, atempoFilters(clip.PlaybackRate)...)
			}
			if clip.VolumeDB != 0 {
				parts = append(parts, "volume="+formatSeconds(clip.VolumeDB)+"dB")
			}
			parts = append(parts, fmt.Sprintf("adelay=%d:all=1", delay))
			filters = append(filters, fmt.Sprintf("[%d:a]%s[%s]", in, strings.Join(parts, ","), label))
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

func opacityExpression(clip SequenceClip, fps int) string {
	base := clip.Opacity
	if base <= 0 && len(clip.OpacityKeys) == 0 {
		base = 1
	}
	if len(clip.OpacityKeys) < 2 {
		return formatSeconds(base)
	}
	keys := append([]SequenceKeyframe(nil), clip.OpacityKeys...)
	sort.Slice(keys, func(i, j int) bool { return keys[i].Frame < keys[j].Frame })
	a, b := keys[0], keys[1]
	start := clip.Start + float64(a.Frame)/float64(fps)
	end := clip.Start + float64(b.Frame)/float64(fps)
	if end <= start {
		return formatSeconds(b.Value)
	}
	return fmt.Sprintf("if(lt(t,%s),%s,if(gte(t,%s),%s,%s+(t-%s)*(%s-%s)/(%s-%s)))", formatSeconds(start), formatSeconds(a.Value), formatSeconds(end), formatSeconds(b.Value), formatSeconds(a.Value), formatSeconds(start), formatSeconds(b.Value), formatSeconds(a.Value), formatSeconds(end), formatSeconds(start))
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
		if clip.Kind != "audio" || clip.Path == "" || clip.Muted {
			continue
		}
		out = append(out, clip)
	}
	return out
}

func atempoFilters(rate float64) []string {
	var out []string
	for rate > 2 {
		out = append(out, "atempo=2")
		rate /= 2
	}
	for rate < .5 {
		out = append(out, "atempo=0.5")
		rate *= 2
	}
	return append(out, "atempo="+formatSeconds(rate))
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

func captionClips(clips []SequenceClip) []SequenceClip {
	var out []SequenceClip
	for _, clip := range clips {
		if clip.Kind == "caption" && strings.TrimSpace(clip.SubtitlePath) != "" {
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
