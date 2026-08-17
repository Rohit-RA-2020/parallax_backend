package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"parallax/internal/ffmpeg"
	"parallax/internal/projects"
	"parallax/internal/transcript"
)

const exportTimeout = 15 * time.Minute

type exportRequest struct {
	Source     string  `json:"source"`
	Format     string  `json:"format"`
	Quality    string  `json:"quality"`
	Resolution string  `json:"resolution"`
	FPS        int     `json:"fps"`
	Audio      *bool   `json:"audio"`
	Start      float64 `json:"start"`
	Duration   float64 `json:"duration"`
	Filename   string  `json:"filename"`
	Captions   string  `json:"captions"`
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	project, err := s.Projects.Get(id)
	if err != nil {
		writeProjectError(w, err)
		return
	}

	var body exportRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	audio := true
	if body.Audio != nil {
		audio = *body.Audio
	}
	spec := ffmpeg.ExportSpec{
		Source:     body.Source,
		Format:     body.Format,
		Quality:    body.Quality,
		Resolution: body.Resolution,
		FPS:        body.FPS,
		Audio:      audio,
		Start:      body.Start,
		Duration:   body.Duration,
		Captions:   body.Captions,
	}
	if err := spec.Normalize(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !spec.IsSequence() {
		if _, err := s.Projects.ResolveFile(id, spec.Source); err != nil {
			writeProjectError(w, err)
			return
		}
	}

	filename := strings.TrimSpace(body.Filename)
	if filename == "" {
		if spec.IsSequence() {
			filename = "sequence-export"
		} else {
			filename = strings.TrimSuffix(filepath.Base(spec.Source), filepath.Ext(spec.Source)) + "-export"
		}
	}
	planned, err := s.Projects.PrepareExport(id, filename, spec.Ext())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var args []string
	if spec.IsSequence() {
		timeline, err := s.Projects.GetTimeline(id)
		if err != nil {
			writeProjectError(w, err)
			return
		}
		clips, tracks, err := prepareSequenceCaptions(project.Dir, sequenceClips(timeline), spec)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		spec.Subtitles = tracks
		args, err = ffmpeg.BuildSequenceArgs(spec, clips, planned.Path)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeExportSidecars(project.Dir, planned.Path, tracks)
	} else {
		if spec.CaptionMode() != "none" {
			if timeline, err := s.Projects.GetTimeline(id); err == nil {
				tracks, err := prepareSourceCaptions(project.Dir, timeline, spec)
				if err != nil {
					writeError(w, http.StatusBadRequest, err.Error())
					return
				}
				spec.Subtitles = tracks
			}
		}
		args, err = ffmpeg.BuildExportArgs(spec, planned.Path)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeExportSidecars(project.Dir, planned.Path, spec.Subtitles)
	}
	cmd, err := ffmpeg.Validate(args, ffmpeg.ValidateOpts{Workspace: project.Dir})
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid export command: "+err.Error())
		return
	}

	res, err := ffmpeg.Run(r.Context(), s.Bins, cmd, project.Dir, exportTimeout)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	_ = res

	media, err := s.Projects.StatFile(id, planned.Path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "export finished but the file is missing")
		return
	}
	_ = s.Projects.Touch(id)

	item := s.mediaResponses(id, []projects.Media{media})[0]
	writeJSON(w, http.StatusCreated, map[string]any{
		"media":        item,
		"download_url": item.ContentURL + downloadQuery(item.ContentURL),
	})
}

func sequenceClips(doc projects.Timeline) []ffmpeg.SequenceClip {
	fps := doc.FPS
	if fps < 1 {
		fps = 24
	}
	rate := float64(fps)
	fadeIn := map[string]projects.TimelineTransition{}
	fadeOut := map[string]projects.TimelineTransition{}
	for _, transition := range doc.Transitions {
		fadeIn[transition.ToID] = transition
		if transition.Type != "crossfade" {
			fadeOut[transition.FromID] = transition
		}
	}
	out := make([]ffmpeg.SequenceClip, 0, len(doc.Clips))
	for _, clip := range doc.Clips {
		item := ffmpeg.SequenceClip{
			Track:        clip.Track,
			Kind:         clip.Kind,
			Path:         clip.MediaPath,
			Name:         clip.Name,
			MediaType:    clip.MediaType,
			Start:        float64(clip.StartFrame) / rate,
			Duration:     float64(clip.DurationFrames) / rate,
			SourceIn:     float64(clip.SourceInFrame) / rate,
			CanvasWidth:  doc.Canvas.Width,
			CanvasHeight: doc.Canvas.Height,
		}
		if clip.Transform != nil {
			item.X = clip.Transform.X
			item.Y = clip.Transform.Y
			item.AnchorX = clip.Transform.AnchorX
			item.AnchorY = clip.Transform.AnchorY
			item.Opacity = clip.Transform.Opacity
			item.ScaleX = clip.Transform.ScaleX
			item.ScaleY = clip.Transform.ScaleY
			item.Rotation = clip.Transform.Rotation
			item.CropTop = clip.Transform.CropTop
			item.CropRight = clip.Transform.CropRight
			item.CropBottom = clip.Transform.CropBottom
			item.CropLeft = clip.Transform.CropLeft
		}
		if clip.Title != nil {
			item.TitleText = clip.Title.Text
			item.FontSize = clip.Title.FontSize
			item.Fill = clip.Title.Fill
		}
		if clip.Kind == "caption" {
			item.SubtitlePath = clip.MediaPath
			if clip.Captions != nil {
				item.CaptionLang = clip.Captions.Language
			}
		}
		if clip.Playback != nil {
			item.PlaybackRate = clip.Playback.Rate
		}
		if clip.Audio != nil {
			item.VolumeDB = clip.Audio.VolumeDB
			item.Muted = clip.Audio.Muted
		}
		if clip.Grade != nil {
			item.Exposure = clip.Grade.Exposure
			item.Contrast = clip.Grade.Contrast
			item.Saturation = clip.Grade.Saturation
		}
		for _, key := range clip.Keyframes {
			if key.Property == "transform.opacity" {
				item.OpacityKeys = append(item.OpacityKeys, ffmpeg.SequenceKeyframe{Frame: key.Frame, Value: key.Value, Easing: key.Easing})
			}
		}
		if transition, ok := fadeIn[clip.ID]; ok {
			item.FadeIn = float64(transition.DurationFrames) / rate
			item.CrossfadeIn = transition.Type == "crossfade"
			if transition.Type == "dip_white" {
				item.FadeColor = "white"
			}
		}
		if transition, ok := fadeOut[clip.ID]; ok {
			item.FadeOut = float64(transition.DurationFrames) / rate
			if transition.Type == "dip_white" {
				item.FadeColor = "white"
			}
		}
		out = append(out, item)
	}
	return out
}

func prepareSequenceCaptions(workspace string, clips []ffmpeg.SequenceClip, spec ffmpeg.ExportSpec) ([]ffmpeg.SequenceClip, []ffmpeg.ExportSubtitle, error) {
	out := append([]ffmpeg.SequenceClip(nil), clips...)
	if spec.CaptionMode() == "none" {
		for i := range out {
			if out[i].Kind == "caption" {
				out[i].SubtitlePath = ""
			}
		}
		return out, nil, nil
	}
	grouped, err := collectProgramCues(workspace, out)
	if err != nil {
		return nil, nil, err
	}
	tracks, err := writeCaptionTracks(workspace, grouped, spec.Format)
	if err != nil {
		return nil, nil, err
	}
	if spec.CaptionMode() == "burn" {
		used := map[string]bool{}
		for i := range out {
			clip := &out[i]
			if clip.Kind != "caption" {
				continue
			}
			lang := captionLangKey(clip.CaptionLang)
			track, ok := trackByLang(tracks, lang)
			if !ok || used[lang] {
				clip.SubtitlePath = ""
				continue
			}
			used[lang] = true
			clip.SubtitlePath = track.Path
			clip.FontName = track.FontName
			clip.FontsDir = track.FontsDir
		}
		return out, nil, nil
	}
	for i := range out {
		if out[i].Kind == "caption" {
			out[i].SubtitlePath = ""
		}
	}
	return out, tracks, nil
}

func prepareSourceCaptions(workspace string, doc projects.Timeline, spec ffmpeg.ExportSpec) ([]ffmpeg.ExportSubtitle, error) {
	source := filepath.ToSlash(strings.TrimSpace(spec.Source))
	grouped := map[string][]transcript.Cue{}
	for _, clip := range doc.Clips {
		if clip.Kind != "caption" {
			continue
		}
		belong := ""
		if clip.Captions != nil {
			belong = filepath.ToSlash(clip.Captions.Source)
		}
		if belong != source {
			continue
		}
		path := strings.TrimSpace(clip.MediaPath)
		if path == "" {
			continue
		}
		abs, err := ffmpeg.ResolveInWorkspace(workspace, path)
		if err != nil {
			return nil, err
		}
		body, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("caption file %s: %w", path, err)
		}
		cues := transcript.ParseSRT(string(body))
		lang := captionLangKey("")
		if clip.Captions != nil {
			lang = captionLangKey(clip.Captions.Language)
		}
		grouped[lang] = append(grouped[lang], cues...)
	}
	if len(grouped) == 0 {
		return nil, nil
	}
	return writeCaptionTracks(workspace, grouped, spec.Format)
}

func collectProgramCues(workspace string, clips []ffmpeg.SequenceClip) (map[string][]transcript.Cue, error) {
	grouped := map[string][]transcript.Cue{}
	for _, clip := range clips {
		if clip.Kind != "caption" || strings.TrimSpace(clip.SubtitlePath) == "" {
			continue
		}
		abs, err := ffmpeg.ResolveInWorkspace(workspace, clip.SubtitlePath)
		if err != nil {
			return nil, err
		}
		body, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("caption file %s: %w", clip.SubtitlePath, err)
		}
		cues := transcript.ParseSRT(string(body))
		cues = transcript.ShiftCues(cues, clip.Start-clip.SourceIn)
		cues = transcript.ClipCues(cues, clip.Start, clip.Duration)
		if len(cues) == 0 {
			continue
		}
		lang := captionLangKey(clip.CaptionLang)
		grouped[lang] = append(grouped[lang], cues...)
	}
	return grouped, nil
}

func writeCaptionTracks(workspace string, grouped map[string][]transcript.Cue, format string) ([]ffmpeg.ExportSubtitle, error) {
	langs := make([]string, 0, len(grouped))
	for lang := range grouped {
		langs = append(langs, lang)
	}
	sort.Strings(langs)
	tracks := make([]ffmpeg.ExportSubtitle, 0, len(langs))
	for _, lang := range langs {
		cues := grouped[lang]
		sort.Slice(cues, func(i, j int) bool {
			if cues[i].Start == cues[j].Start {
				return cues[i].End < cues[j].End
			}
			return cues[i].Start < cues[j].Start
		})
		if len(cues) == 0 {
			continue
		}
		ext := ".srt"
		body := transcript.WriteSRT(cues)
		if format == "webm" {
			ext = ".vtt"
			body = transcript.WriteVTT(cues)
		}
		rel := filepath.ToSlash(filepath.Join(".scratch", "export-cap-"+lang+ext))
		dest, err := ffmpeg.ResolveInWorkspace(workspace, rel)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dest, []byte(body), 0o644); err != nil {
			return nil, err
		}
		font, err := ffmpeg.StageCaptionFont(workspace, lang)
		if err != nil {
			return nil, err
		}
		tracks = append(tracks, ffmpeg.ExportSubtitle{
			Path:     rel,
			Language: transcript.CaptionLangISO6392(lang),
			Title:    transcript.CaptionLanguageName(lang),
			FontName: font.Name,
			FontsDir: font.FontsDir,
			FontSize: 32,
		})
	}
	return tracks, nil
}

func writeExportSidecars(workspace, dest string, tracks []ffmpeg.ExportSubtitle) {
	if len(tracks) == 0 {
		return
	}
	base := strings.TrimSuffix(dest, filepath.Ext(dest))
	for _, track := range tracks {
		src, err := ffmpeg.ResolveInWorkspace(workspace, track.Path)
		if err != nil {
			continue
		}
		body, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		code := transcript.NormalizeCaptionLang(track.Language)
		if code == "" || code == "original" {
			code = "und"
		}
		rel := filepath.ToSlash(base + "." + code + filepath.Ext(track.Path))
		out, err := ffmpeg.ResolveInWorkspace(workspace, rel)
		if err != nil {
			continue
		}
		_ = os.WriteFile(out, body, 0o644)
	}
}

func captionLangKey(lang string) string {
	lang = transcript.NormalizeCaptionLang(lang)
	if lang == "" || lang == "original" {
		return "und"
	}
	return lang
}

func trackByLang(tracks []ffmpeg.ExportSubtitle, lang string) (ffmpeg.ExportSubtitle, bool) {
	want := transcript.CaptionLangISO6392(lang)
	for _, track := range tracks {
		if track.Language == want || transcript.NormalizeCaptionLang(track.Language) == lang {
			return track, true
		}
	}
	return ffmpeg.ExportSubtitle{}, false
}

func downloadQuery(contentURL string) string {
	if strings.Contains(contentURL, "?") {
		return "&download=1"
	}
	return "?download=1"
}
