package transcript

import (
	"math"
	"sort"
	"strings"
)

const (
	minSceneLen      = 0.4
	sceneSampleEvery = 3.5
	maxScenesPerFile = 48
	sceneFramePad    = 0.35
)

// SceneWindow is a visual span to caption and embed.
type SceneWindow struct {
	Start float64
	End   float64
	At    float64
}

// PlanSceneWindows turns cut times plus duration into caption windows.
// Cuts are visual only. Long shots are split on a clock so a locked-off
// take still gets more than one description.
func PlanSceneWindows(cuts []float64, duration float64) []SceneWindow {
	return planSceneWindows(cuts, duration, sceneSampleEvery, minSceneLen, maxScenesPerFile)
}

func planSceneWindows(cuts []float64, duration, every, minLen float64, maxN int) []SceneWindow {
	if duration < 0 {
		duration = 0
	}
	if every < minLen {
		every = minLen
	}
	bounds := normalizeCutBounds(cuts, duration, minLen)
	var windows []SceneWindow
	for i := 0; i < len(bounds)-1; i++ {
		start, end := bounds[i], bounds[i+1]
		if end-start < 0.05 {
			continue
		}
		if end-start <= every+0.25 {
			windows = append(windows, SceneWindow{Start: start, End: end, At: sceneFrameTime(start, end)})
			continue
		}
		t := start
		for t < end-0.05 {
			next := math.Min(t+every, end)
			if next-t < minLen && len(windows) > 0 && windows[len(windows)-1].Start >= start-1e-6 {
				windows[len(windows)-1].End = end
				break
			}
			windows = append(windows, SceneWindow{Start: t, End: next, At: sceneFrameTime(t, next)})
			if next >= end-1e-6 {
				break
			}
			t = next
		}
	}
	if len(windows) == 0 {
		windows = []SceneWindow{{Start: 0, End: duration, At: sceneFrameTime(0, duration)}}
	}
	if maxN > 0 && len(windows) > maxN {
		windows = downsampleWindows(windows, maxN)
	}
	return windows
}

func normalizeCutBounds(cuts []float64, duration, minLen float64) []float64 {
	pts := []float64{0}
	sorted := append([]float64(nil), cuts...)
	sort.Float64s(sorted)
	for _, cut := range sorted {
		if math.IsNaN(cut) || cut < minLen {
			continue
		}
		if duration > 0 && cut >= duration-0.05 {
			continue
		}
		if cut-pts[len(pts)-1] < minLen {
			continue
		}
		pts = append(pts, cut)
	}
	end := duration
	if end <= pts[len(pts)-1] {
		end = pts[len(pts)-1]
		if end < 0.1 {
			end = 0.1
		}
	}
	if end-pts[len(pts)-1] < 0.05 {
		pts[len(pts)-1] = end
	} else {
		pts = append(pts, end)
	}
	return pts
}

func sceneFrameTime(start, end float64) float64 {
	if end <= start {
		return start
	}
	at := start + sceneFramePad
	if at >= end {
		return start + (end-start)*0.4
	}
	return at
}

func downsampleWindows(windows []SceneWindow, maxN int) []SceneWindow {
	if maxN < 1 || len(windows) <= maxN {
		return windows
	}
	out := make([]SceneWindow, 0, maxN)
	if maxN == 1 {
		return []SceneWindow{windows[0]}
	}
	last := len(windows) - 1
	for i := 0; i < maxN; i++ {
		idx := int(math.Round(float64(i) * float64(last) / float64(maxN-1)))
		if len(out) > 0 && idx <= 0 {
			idx = 1
		}
		if i == maxN-1 {
			idx = last
		}
		out = append(out, windows[idx])
	}
	return out
}

func spokenInRange(segments []Segment, start, end float64) string {
	var parts []string
	for _, seg := range segments {
		if seg.End <= start || seg.Start >= end {
			continue
		}
		if t := strings.TrimSpace(seg.TextEN); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}
