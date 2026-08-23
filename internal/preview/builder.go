package preview

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"parallax/internal/ffmpeg"
	"parallax/internal/projects"
)

const (
	queueSize          = 32
	jobTimeout         = 4 * time.Hour
	posterSeekSec      = 2.0
	timelineFrameLimit = 12
)

// Builder builds H.264 preview proxies so the browser can play MKV/HEVC/10-bit files.
type Builder struct {
	Projects *projects.Store
	Bins     ffmpeg.Bins
	Logger   *slog.Logger

	mu      sync.Mutex
	diskMu  sync.Mutex
	live    map[string]Status
	queue   chan previewJob
	stop    chan struct{}
	cancel  context.CancelFunc
	ctx     context.Context
	run     bool
	wg      sync.WaitGroup
	pending map[string]bool
}

type previewJob struct {
	projectID string
	rel       string
	enqueued  time.Time
}

func (b *Builder) log() *slog.Logger {
	if b != nil && b.Logger != nil {
		return b.Logger
	}
	return slog.Default()
}

// Start the serial preview worker.
func (b *Builder) Start() {
	if b == nil || b.Projects == nil {
		return
	}
	b.mu.Lock()
	if b.run {
		b.mu.Unlock()
		return
	}
	b.queue = make(chan previewJob, queueSize)
	b.stop = make(chan struct{})
	b.ctx, b.cancel = context.WithCancel(context.Background())
	b.run = true
	b.mu.Unlock()
	b.wg.Add(2)
	go b.loop()
	go b.recoveryLoop()
}

// Close stops the worker.
func (b *Builder) Close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	if b.run && b.stop != nil {
		close(b.stop)
		b.run = false
	}
	if b.cancel != nil {
		b.cancel()
	}
	b.mu.Unlock()
	b.wg.Wait()
}

// Enqueue schedules a preview for a project-relative video.
func (b *Builder) Enqueue(projectID, rel string) {
	if b == nil || b.Projects == nil {
		return
	}
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if rel == "" || !ffmpeg.HasVideoExt(rel) {
		return
	}
	b.Start()
	b.mu.Lock()
	alreadyPending := b.pending[previewPendingKey(projectID, rel)]
	b.mu.Unlock()
	if alreadyPending {
		return
	}
	plan := ffmpeg.PreviewEncodePlan(b.Bins)
	enqueued := time.Now().UTC()
	b.Mark(projectID, rel, Status{
		State:     StateQueued,
		Encoder:   plan.Encoder,
		Device:    plan.Device,
		Hardware:  plan.Hardware,
		Pipeline:  plan.Pipeline,
		StartedAt: enqueued,
	})
	b.tryEnqueue(previewJob{projectID: projectID, rel: rel, enqueued: enqueued})
}

func previewPendingKey(projectID, rel string) string { return projectID + "\n" + rel }

func (b *Builder) tryEnqueue(job previewJob) {
	key := previewPendingKey(job.projectID, job.rel)
	b.mu.Lock()
	if b.pending == nil {
		b.pending = map[string]bool{}
	}
	if b.pending[key] {
		b.mu.Unlock()
		return
	}
	b.pending[key] = true
	b.mu.Unlock()
	select {
	case b.queue <- job:
	case <-b.stop:
		b.clearPending(job)
	default:
		b.clearPending(job)
	}
}

func (b *Builder) clearPending(job previewJob) {
	b.mu.Lock()
	delete(b.pending, previewPendingKey(job.projectID, job.rel))
	b.mu.Unlock()
}

func (b *Builder) loop() {
	defer b.wg.Done()
	for {
		select {
		case <-b.stop:
			return
		case job := <-b.queue:
			b.runJob(job)
			b.clearPending(job)
		}
	}
}

func (b *Builder) recoveryLoop() {
	defer b.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	b.recoverPending()
	for {
		select {
		case <-b.stop:
			return
		case <-ticker.C:
			b.recoverPending()
		}
	}
}

func (b *Builder) recoverPending() {
	if b == nil || b.Projects == nil {
		return
	}
	for _, project := range b.Projects.List() {
		for rel, st := range readStatusFile(project.Dir) {
			if st.State != StateQueued && st.State != StateBuilding {
				continue
			}
			key := previewPendingKey(project.ID, rel)
			b.mu.Lock()
			pending := b.pending[key]
			b.mu.Unlock()
			if !pending {
				b.Enqueue(project.ID, rel)
			}
		}
	}
}

// QueueDepth reports preview jobs buffered or currently owned by the worker.
func (b *Builder) QueueDepth() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending)
}

func (b *Builder) runJob(job previewJob) {
	parent := context.Background()
	if b.ctx != nil {
		parent = b.ctx
	}
	ctx, cancel := context.WithTimeout(parent, jobTimeout)
	defer cancel()
	if err := b.build(ctx, job.projectID, job.rel, job.enqueued); err != nil {
		if b.Projects != nil {
			if _, getErr := b.Projects.Get(job.projectID); getErr != nil {
				return
			}
		}
		plan := ffmpeg.PreviewEncodePlan(b.Bins)
		b.Mark(job.projectID, job.rel, Status{
			State:    StateFailed,
			Error:    err.Error(),
			Encoder:  plan.Encoder,
			Device:   plan.Device,
			Hardware: plan.Hardware,
			Pipeline: plan.Pipeline,
		})
		b.log().Error("preview proxy", "project", job.projectID, "path", job.rel, "err", err)
	}
}

// Build writes a browser-playable proxy when the source cannot play natively.
func (b *Builder) Build(ctx context.Context, projectID, rel string) error {
	return b.build(ctx, projectID, rel, time.Now().UTC())
}

func (b *Builder) build(ctx context.Context, projectID, rel string, enqueued time.Time) error {
	if b == nil || b.Projects == nil {
		return nil
	}
	if enqueued.IsZero() {
		enqueued = time.Now().UTC()
	}
	buildStarted := time.Now()
	timings := Timings{QueueMs: elapsedMs(enqueued)}
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	project, err := b.Projects.Get(projectID)
	if err != nil {
		return err
	}
	abs, err := b.Projects.ResolveFile(projectID, rel)
	if err != nil {
		return err
	}
	probeStarted := time.Now()
	info, err := ffmpeg.ProbeMedia(ctx, b.Bins, project.Dir, rel)
	if err != nil {
		return err
	}
	timings.ProbeMs = elapsedMs(probeStarted)
	if !info.HasVideo {
		return nil
	}
	key := previewKey(rel, abs)
	proxyRel := filepath.ToSlash(filepath.Join(".parallax", "previews", key+".mp4"))
	posterRel := filepath.ToSlash(filepath.Join(".parallax", "previews", key+".jpg"))
	codec := strings.TrimSpace(info.VideoCodec)
	reason := ffmpeg.PreviewReason(rel, info)
	plan := ffmpeg.PreviewEncodePlanForSource(b.Bins, codec)
	frameCount := timelineFrameCount(info.Duration)
	existingFrames := existingTimelineFrames(project.Dir, key, frameCount)

	if st, err := os.Stat(filepath.Join(project.Dir, filepath.FromSlash(proxyRel))); err == nil && st.Size() > 0 {
		poster := existingPoster(project.Dir, posterRel)
		if poster == "" && len(existingFrames) > 0 {
			poster = existingFrames[0]
		}
		status := Status{
			State:           StateReady,
			URLPath:         proxyRel,
			PosterPath:      poster,
			TimelineFrames:  existingFrames,
			Codec:           codec,
			Reason:          reason,
			TimelinePending: len(existingFrames) < frameCount,
			TimelineReady:   len(existingFrames) >= frameCount,
		}
		b.Mark(projectID, rel, status)
		if len(existingFrames) < frameCount {
			status.TimelineFrames = b.buildTimelineFrames(ctx, project.Dir, rel, key, info.Duration)
			if status.PosterPath == "" && len(status.TimelineFrames) > 0 {
				status.PosterPath = status.TimelineFrames[0]
			}
			status.TimelinePending = false
			status.TimelineReady = true
			b.Mark(projectID, rel, status)
		}
		return nil
	}

	if ffmpeg.BrowserPlayable(rel, info) {
		status := Status{State: StateOriginal, Codec: codec, TimelineFrames: existingFrames, TimelinePending: len(existingFrames) < frameCount, TimelineReady: len(existingFrames) >= frameCount}
		if len(existingFrames) > 0 {
			status.PosterPath = existingFrames[0]
		}
		// Make the original playable immediately. Filmstrip generation continues
		// within this serial background job and never blocks media playback.
		b.Mark(projectID, rel, status)
		if len(existingFrames) < frameCount {
			status.TimelineFrames = b.buildTimelineFrames(ctx, project.Dir, rel, key, info.Duration)
			if len(status.TimelineFrames) > 0 {
				status.PosterPath = status.TimelineFrames[0]
			}
			status.TimelinePending = false
			status.TimelineReady = true
			b.Mark(projectID, rel, status)
		}
		return nil
	}

	b.Mark(projectID, rel, Status{
		State: StateBuilding, Reason: reason, Codec: codec, Progress: "poster",
		Encoder: plan.Encoder, Device: plan.Device, Hardware: plan.Hardware, Pipeline: plan.Pipeline,
		Timings: timings, StartedAt: enqueued,
	})
	posterAt := posterSeekSec
	if info.Duration > 0 && info.Duration < posterAt {
		posterAt = info.Duration / 3
	}
	posterStarted := time.Now()
	if err := ffmpeg.ExtractFrame(ctx, b.Bins, project.Dir, rel, posterRel, posterAt); err != nil {
		b.log().Info("preview poster", "path", rel, "err", err)
		posterRel = ""
	}
	timings.PosterMs = elapsedMs(posterStarted)
	timelineFrames := b.buildTimelineFrames(ctx, project.Dir, rel, key, info.Duration)

	b.Mark(projectID, rel, Status{
		State:          StateBuilding,
		PosterPath:     posterRel,
		TimelineFrames: timelineFrames,
		TimelineReady:  true,
		Reason:         reason,
		Codec:          codec,
		Progress:       "0%",
		Encoder:        plan.Encoder,
		Device:         plan.Device,
		Hardware:       plan.Hardware,
		Pipeline:       plan.Pipeline,
		Timings:        timings,
		StartedAt:      enqueued,
	})
	transcodeStarted := time.Now()
	encoded, err := ffmpeg.WritePreviewWithSourceCodec(ctx, b.Bins, project.Dir, rel, proxyRel, info.Duration, codec, func(at, total float64) {
		progress := ""
		if total > 0 {
			pct := int(at / total * 100)
			if pct > 99 {
				pct = 99
			}
			if pct < 0 {
				pct = 0
			}
			progress = fmt.Sprintf("%d%% · %s / %s", pct, formatClock(at), formatClock(total))
		} else if at > 0 {
			progress = formatClock(at)
		}
		liveTimings := timings
		liveTimings.TranscodeMs = elapsedMs(transcodeStarted)
		liveTimings.TotalMs = elapsedMs(buildStarted) + timings.QueueMs
		b.Mark(projectID, rel, Status{
			State:      StateBuilding,
			PosterPath: posterRel,
			Reason:     reason,
			Codec:      codec,
			Progress:   progress,
			Encoder:    plan.Encoder,
			Device:     plan.Device,
			Hardware:   plan.Hardware,
			Pipeline:   plan.Pipeline,
			Timings:    liveTimings,
			StartedAt:  enqueued,
		})
	})
	if err != nil {
		return err
	}
	timings.TranscodeMs = elapsedMs(transcodeStarted)
	timings.TotalMs = elapsedMs(buildStarted) + timings.QueueMs
	b.Mark(projectID, rel, Status{
		State:          StateReady,
		URLPath:        proxyRel,
		PosterPath:     posterRel,
		TimelineFrames: timelineFrames,
		TimelineReady:  true,
		Reason:         reason,
		Codec:          codec,
		Encoder:        encoded.Encoder,
		Device:         encoded.Device,
		Hardware:       encoded.Hardware,
		Pipeline:       encoded.Pipeline,
		Timings:        timings,
		StartedAt:      enqueued,
	})
	b.log().Info("preview ready", "path", rel, "proxy", proxyRel, "codec", codec, "encoder", encoded.Encoder, "device", encoded.Device, "pipeline", encoded.Pipeline)
	return nil
}

func timelineFrameCount(duration float64) int {
	if duration <= 0 {
		return 0
	}
	count := int(math.Ceil(duration / 15))
	if count < 4 {
		count = 4
	}
	if count > timelineFrameLimit {
		count = timelineFrameLimit
	}
	return count
}

func timelineFrameRel(key string, index int) string {
	return filepath.ToSlash(filepath.Join(".parallax", "previews", fmt.Sprintf("%s-timeline-%02d.jpg", key, index)))
}

func existingTimelineFrames(projectDir, key string, count int) []string {
	frames := make([]string, 0, count)
	for i := 0; i < count; i++ {
		rel := timelineFrameRel(key, i)
		if st, err := os.Stat(filepath.Join(projectDir, filepath.FromSlash(rel))); err == nil && st.Size() > 0 {
			frames = append(frames, rel)
		}
	}
	return frames
}

func (b *Builder) buildTimelineFrames(ctx context.Context, projectDir, sourceRel, key string, duration float64) []string {
	count := timelineFrameCount(duration)
	if count == 0 {
		return nil
	}
	frames := make([]string, 0, count)
	for i := 0; i < count; i++ {
		if err := ctx.Err(); err != nil {
			break
		}
		rel := timelineFrameRel(key, i)
		abs := filepath.Join(projectDir, filepath.FromSlash(rel))
		if st, err := os.Stat(abs); err == nil && st.Size() > 0 {
			frames = append(frames, rel)
			continue
		}
		at := duration * (float64(i) + 0.5) / float64(count)
		if err := ffmpeg.ExtractThumbnail(ctx, b.Bins, projectDir, sourceRel, rel, at); err != nil {
			b.log().Info("timeline thumbnail", "path", sourceRel, "at", at, "err", err)
			continue
		}
		frames = append(frames, rel)
	}
	return frames
}

func elapsedMs(start time.Time) int64 {
	if start.IsZero() {
		return 0
	}
	ms := time.Since(start).Milliseconds()
	if ms < 1 {
		return 1
	}
	return ms
}

func previewKey(rel, abs string) string {
	info, err := os.Stat(abs)
	sum := sha1.New()
	_, _ = fmt.Fprintf(sum, "%s\n", filepath.ToSlash(rel))
	if err == nil {
		_, _ = fmt.Fprintf(sum, "%d\n%d\n", info.Size(), info.ModTime().UnixNano())
	}
	return hex.EncodeToString(sum.Sum(nil))[:20]
}

func existingPoster(projectDir, rel string) string {
	if rel == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(projectDir, filepath.FromSlash(rel))); err == nil {
		return rel
	}
	return ""
}
