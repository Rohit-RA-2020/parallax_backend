package preview

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"parallax/internal/ffmpeg"
	"parallax/internal/projects"
)

const (
	queueSize     = 32
	jobTimeout    = 4 * time.Hour
	posterSeekSec = 2.0
)

// Builder builds H.264 preview proxies so the browser can play MKV/HEVC/10-bit files.
type Builder struct {
	Projects *projects.Store
	Bins     ffmpeg.Bins
	Logger   *slog.Logger

	mu     sync.Mutex
	diskMu sync.Mutex
	live   map[string]Status
	queue  chan previewJob
	stop   chan struct{}
	cancel context.CancelFunc
	ctx    context.Context
	run    bool
	wg     sync.WaitGroup
}

type previewJob struct {
	projectID string
	rel       string
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
	b.wg.Add(1)
	go b.loop()
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
	plan := ffmpeg.PreviewEncodePlan(b.Bins)
	b.Mark(projectID, rel, Status{
		State:    StateQueued,
		Encoder:  plan.Encoder,
		Device:   plan.Device,
		Hardware: plan.Hardware,
	})
	select {
	case b.queue <- previewJob{projectID: projectID, rel: rel}:
	case <-b.stop:
	}
}

func (b *Builder) loop() {
	defer b.wg.Done()
	for {
		select {
		case <-b.stop:
			return
		case job := <-b.queue:
			b.runJob(job)
		}
	}
}

func (b *Builder) runJob(job previewJob) {
	parent := context.Background()
	if b.ctx != nil {
		parent = b.ctx
	}
	ctx, cancel := context.WithTimeout(parent, jobTimeout)
	defer cancel()
	if err := b.Build(ctx, job.projectID, job.rel); err != nil {
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
		})
		b.log().Error("preview proxy", "project", job.projectID, "path", job.rel, "err", err)
	}
}

// Build writes a browser-playable proxy when the source cannot play natively.
func (b *Builder) Build(ctx context.Context, projectID, rel string) error {
	if b == nil || b.Projects == nil {
		return nil
	}
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	project, err := b.Projects.Get(projectID)
	if err != nil {
		return err
	}
	abs, err := b.Projects.ResolveFile(projectID, rel)
	if err != nil {
		return err
	}
	info, err := ffmpeg.ProbeMedia(ctx, b.Bins, project.Dir, rel)
	if err != nil {
		return err
	}
	if !info.HasVideo {
		return nil
	}
	key := previewKey(rel, abs)
	proxyRel := filepath.ToSlash(filepath.Join(".parallax", "previews", key+".mp4"))
	posterRel := filepath.ToSlash(filepath.Join(".parallax", "previews", key+".jpg"))
	codec := strings.TrimSpace(info.VideoCodec)
	reason := ffmpeg.PreviewReason(rel, info)
	plan := ffmpeg.PreviewEncodePlan(b.Bins)

	if st, err := os.Stat(filepath.Join(project.Dir, filepath.FromSlash(proxyRel))); err == nil && st.Size() > 0 {
		b.Mark(projectID, rel, Status{
			State:      StateReady,
			URLPath:    proxyRel,
			PosterPath: existingPoster(project.Dir, posterRel),
			Codec:      codec,
			Reason:     reason,
		})
		return nil
	}

	if ffmpeg.BrowserPlayable(rel, info) {
		b.Mark(projectID, rel, Status{State: StateOriginal, Codec: codec})
		return nil
	}

	b.Mark(projectID, rel, Status{
		State: StateBuilding, Reason: reason, Codec: codec, Progress: "poster",
		Encoder: plan.Encoder, Device: plan.Device, Hardware: plan.Hardware,
	})
	posterAt := posterSeekSec
	if info.Duration > 0 && info.Duration < posterAt {
		posterAt = info.Duration / 3
	}
	if err := ffmpeg.ExtractFrame(ctx, b.Bins, project.Dir, rel, posterRel, posterAt); err != nil {
		b.log().Info("preview poster", "path", rel, "err", err)
		posterRel = ""
	}

	b.Mark(projectID, rel, Status{
		State:      StateBuilding,
		PosterPath: posterRel,
		Reason:     reason,
		Codec:      codec,
		Progress:   "0%",
		Encoder:    plan.Encoder,
		Device:     plan.Device,
		Hardware:   plan.Hardware,
	})
	encoded, err := ffmpeg.WritePreviewWithInfo(ctx, b.Bins, project.Dir, rel, proxyRel, info.Duration, func(at, total float64) {
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
		b.Mark(projectID, rel, Status{
			State:      StateBuilding,
			PosterPath: posterRel,
			Reason:     reason,
			Codec:      codec,
			Progress:   progress,
			Encoder:    plan.Encoder,
			Device:     plan.Device,
			Hardware:   plan.Hardware,
		})
	})
	if err != nil {
		return err
	}
	b.Mark(projectID, rel, Status{
		State:      StateReady,
		URLPath:    proxyRel,
		PosterPath: posterRel,
		Reason:     reason,
		Codec:      codec,
		Encoder:    encoded.Encoder,
		Device:     encoded.Device,
		Hardware:   encoded.Hardware,
	})
	b.log().Info("preview ready", "path", rel, "proxy", proxyRel, "codec", codec, "encoder", encoded.Encoder, "device", encoded.Device)
	return nil
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
