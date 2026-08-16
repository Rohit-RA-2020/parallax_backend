package httpapi

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"parallax/internal/ffmpeg"
	"parallax/internal/projects"
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
		args, err = ffmpeg.BuildSequenceArgs(spec, sequenceClips(timeline), planned.Path)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		args, err = ffmpeg.BuildExportArgs(spec, planned.Path)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
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

func downloadQuery(contentURL string) string {
	if strings.Contains(contentURL, "?") {
		return "&download=1"
	}
	return "?download=1"
}
