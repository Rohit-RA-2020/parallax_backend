package transcript

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"parallax/internal/embed"
	"parallax/internal/ffmpeg"
	"parallax/internal/llm"
	"parallax/internal/projects"
	"parallax/internal/qdrant"
)

// Indexer transcribes imported media, translates segments, and upserts vectors.
type Indexer struct {
	Projects   *projects.Store
	Bins       ffmpeg.Bins
	Whisper    Transcriber
	Embeddings *embed.Client
	Qdrant     *qdrant.Client
	Completer  func() llm.Completer
	Logger     *slog.Logger
	// ImageWorkers is how many stills to caption at once. Zero means 6.
	// Speech and video stay serial so the GPU is not shared.
	ImageWorkers int

	mu      sync.Mutex
	diskMu  sync.Mutex
	live    map[string]JobStatus
	hints   map[string]string
	queue   chan indexJob
	light   chan indexJob
	stop    chan struct{}
	workers sync.WaitGroup
	run     bool
}

type indexJob struct {
	projectID string
	rel       string
	describe  bool
}

func (x *Indexer) log() *slog.Logger {
	if x != nil && x.Logger != nil {
		return x.Logger
	}
	return slog.Default()
}

// Enabled is true when speech or still indexing can run. Embeddings may still be skipped.
func (x *Indexer) Enabled() bool {
	return x != nil && x.Projects != nil && (x.Whisper != nil || x.canCaption())
}

const (
	defaultImageWorkers = 6
	indexQueueSize      = 128
)

func (x *Indexer) imageWorkers() int {
	if x != nil && x.ImageWorkers > 0 {
		return x.ImageWorkers
	}
	return defaultImageWorkers
}

// Start warms the whisper worker, one serial media loop, and a stills pool.
func (x *Indexer) Start() {
	if x == nil || x.Projects == nil {
		return
	}
	x.mu.Lock()
	if x.run {
		x.mu.Unlock()
		return
	}
	x.queue = make(chan indexJob, indexQueueSize)
	x.light = make(chan indexJob, indexQueueSize)
	x.stop = make(chan struct{})
	x.run = true
	n := x.imageWorkers()
	x.mu.Unlock()
	if w, ok := x.Whisper.(*FasterWhisper); ok {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			if err := w.Ensure(ctx); err != nil {
				x.log().Info("whisper worker will start on first file", "err", err)
			}
		}()
	}
	x.workers.Add(1 + n)
	go x.loop()
	for i := 0; i < n; i++ {
		go x.lightLoop()
	}
}

// Enqueue schedules a file. Stills go to a parallel caption pool; speech and
// video stay on the serial queue so Whisper keeps the GPU to itself.
func (x *Indexer) Enqueue(projectID, rel string) {
	if x == nil || x.Projects == nil {
		return
	}
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if rel == "" {
		return
	}
	switch {
	case HasImage(rel):
		if !x.canCaption() {
			return
		}
	case HasVideo(rel):
		if x.Whisper == nil && !x.canCaption() {
			return
		}
	case hasAudioExt(rel):
		if x.Whisper == nil {
			return
		}
	default:
		return
	}
	x.Start()
	x.Mark(projectID, rel, StateQueued, "")
	dest := x.queue
	if HasImage(rel) {
		dest = x.light
	}
	select {
	case dest <- indexJob{projectID: projectID, rel: rel}:
	case <-x.stop:
	}
}

// EnqueueDescribe schedules visual scene captions even when a transcript exists.
func (x *Indexer) EnqueueDescribe(projectID, rel string) {
	if x == nil || x.Projects == nil || !x.canCaption() || !HasVideo(rel) {
		return
	}
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if rel == "" {
		return
	}
	x.Start()
	x.Mark(projectID, rel, StateQueued, "")
	select {
	case x.queue <- indexJob{projectID: projectID, rel: rel, describe: true}:
	case <-x.stop:
	}
}

func (x *Indexer) loop() {
	defer x.workers.Done()
	for {
		select {
		case <-x.stop:
			return
		case job := <-x.queue:
			x.runJob(job)
		}
	}
}

func (x *Indexer) lightLoop() {
	defer x.workers.Done()
	for {
		select {
		case <-x.stop:
			return
		case job := <-x.light:
			x.runJob(job)
		}
	}
}

func (x *Indexer) runJob(job indexJob) {
	x.stampQueue(job.projectID, job.rel)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	if err := x.index(ctx, job.projectID, job.rel, job.describe); err != nil {
		if x.Projects != nil {
			if _, getErr := x.Projects.Get(job.projectID); getErr != nil {
				x.clearProject(job.projectID)
				return
			}
		}
		x.Mark(job.projectID, job.rel, StateFailed, err.Error())
		x.log().Error("index media", "project", job.projectID, "path", job.rel, "err", err)
	}
}

// Close stops the queues and the resident whisper worker.
func (x *Indexer) Close() {
	if x == nil {
		return
	}
	x.mu.Lock()
	if x.run && x.stop != nil {
		close(x.stop)
		x.run = false
	}
	x.mu.Unlock()
	x.workers.Wait()
	if w, ok := x.Whisper.(*FasterWhisper); ok {
		w.Close()
	}
}

// Index transcribes or captions one project-relative media file, then embeds it.
func (x *Indexer) Index(ctx context.Context, projectID, rel string) error {
	return x.index(ctx, projectID, rel, false)
}

// IndexDescribe transcribes if needed, then always runs visual scene captions.
func (x *Indexer) IndexDescribe(ctx context.Context, projectID, rel string) error {
	return x.index(ctx, projectID, rel, true)
}

func (x *Indexer) index(ctx context.Context, projectID, rel string, describe bool) error {
	if x == nil || x.Projects == nil {
		return nil
	}
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if rel == "" {
		return fmt.Errorf("media path is required")
	}
	if HasImage(rel) {
		return x.indexImage(ctx, projectID, rel)
	}
	var speech *Document
	var first error
	if x.Whisper != nil && hasAudioExt(rel) {
		doc, err := x.indexSpeech(ctx, projectID, rel)
		speech = doc
		if err != nil {
			first = err
		}
	}
	hasWords := speech != nil && len(speech.Segments) > 0
	if x.canCaption() && HasVideo(rel) {
		if describe || !hasWords {
			if err := x.indexScenes(ctx, projectID, rel); err != nil {
				if first == nil {
					first = err
				}
			} else {
				x.patchStatus(projectID, rel, true, func(st *JobStatus) {
					st.CanDescribe = false
				})
			}
		} else {
			x.patchStatus(projectID, rel, true, func(st *JobStatus) {
				st.CanDescribe = true
			})
		}
	}
	return first
}

func (x *Indexer) indexSpeech(ctx context.Context, projectID, rel string) (*Document, error) {
	project, err := x.Projects.Get(projectID)
	if err != nil {
		return nil, err
	}
	abs, err := x.Projects.ResolveFile(projectID, rel)
	if err != nil {
		return nil, err
	}
	if !hasAudioExt(rel) {
		x.Mark(projectID, rel, StateSkipped, "")
		return nil, nil
	}
	info, err := ffmpeg.ProbeMedia(ctx, x.Bins, project.Dir, rel)
	if err != nil {
		return nil, err
	}
	if !info.HasAudio {
		x.Mark(projectID, rel, StateSkipped, "")
		return nil, nil
	}
	if info.Duration > 0 {
		x.patchStatus(projectID, rel, false, func(st *JobStatus) {
			st.Duration = info.Duration
		})
	}
	hash, err := projects.HashFile(abs)
	if err != nil {
		return nil, err
	}

	doc, err := Load(project.Dir, hash)
	if err != nil {
		return nil, err
	}
	if complete(doc) && (!x.canEmbed() || doc.Embedded) {
		x.NoteCached(projectID, rel)
		x.Mark(projectID, rel, StateReady, "")
		return doc, nil
	}

	if doc == nil || len(doc.Segments) == 0 {
		x.Mark(projectID, rel, StateTranscribing, "")
		doc, err = x.transcribe(ctx, projectID, project.Dir, rel, hash, info.Duration)
		if err != nil {
			return nil, err
		}
	} else {
		doc.Path = rel
		x.NoteCached(projectID, rel)
	}

	if needsEnglish(doc.Segments) {
		x.Mark(projectID, rel, StateTranslating, "")
		started := time.Now()
		if err := x.ensureEnglish(ctx, doc); err != nil {
			_ = Save(project.Dir, doc)
			return doc, err
		}
		x.AddTiming(projectID, rel, TimingTranslate, sinceMs(started))
	} else if err := x.ensureEnglish(ctx, doc); err != nil {
		_ = Save(project.Dir, doc)
		return doc, err
	}
	if err := Save(project.Dir, doc); err != nil {
		return doc, err
	}
	if !x.canEmbed() {
		x.Mark(projectID, rel, StateReady, "")
		x.logTimings(projectID, rel)
		return doc, nil
	}
	if doc.Embedded {
		x.Mark(projectID, rel, StateReady, "")
		x.logTimings(projectID, rel)
		return doc, nil
	}
	x.Mark(projectID, rel, StateIndexing, "")
	started := time.Now()
	if err := x.upsert(ctx, projectID, doc); err != nil {
		doc.Embedded = false
		_ = Save(project.Dir, doc)
		x.AddTiming(projectID, rel, TimingIndex, sinceMs(started))
		x.Mark(projectID, rel, StateIndexFailed, err.Error())
		x.log().Error("transcript embed", "project", projectID, "path", rel, "err", err)
		return doc, nil
	}
	x.AddTiming(projectID, rel, TimingIndex, sinceMs(started))
	doc.Embedded = true
	if err := Save(project.Dir, doc); err != nil {
		return doc, err
	}
	x.Mark(projectID, rel, StateReady, "")
	x.logTimings(projectID, rel)
	return doc, nil
}

func needsEnglish(segments []Segment) bool {
	for _, seg := range segments {
		if strings.TrimSpace(seg.Text) != "" && strings.TrimSpace(seg.TextEN) == "" {
			return true
		}
	}
	return false
}

func complete(doc *Document) bool {
	return doc != nil && len(doc.Segments) > 0 && !needsEnglish(doc.Segments)
}

func (x *Indexer) canEmbed() bool {
	return x != nil && x.Embeddings != nil && x.Qdrant != nil
}

func (x *Indexer) transcribe(ctx context.Context, projectID, projectDir, rel, hash string, duration float64) (*Document, error) {
	scratch := filepath.ToSlash(filepath.Join(".scratch", "asr-"+hash+".wav"))
	extractStarted := time.Now()
	if err := ffmpeg.ExtractMono16k(ctx, x.Bins, projectDir, rel, scratch); err != nil {
		return nil, err
	}
	x.AddTiming(projectID, rel, TimingExtract, sinceMs(extractStarted))
	wavAbs := filepath.Join(projectDir, filepath.FromSlash(scratch))
	defer os.Remove(wavAbs)

	audioHash, err := projects.HashFile(wavAbs)
	if err != nil {
		return nil, err
	}
	if reused, err := FindByAudioHash(projectDir, audioHash); err != nil {
		return nil, err
	} else if reused != nil {
		reused.ContentHash = hash
		reused.Path = rel
		reused.AudioHash = audioHash
		reused.Duration = duration
		reused.Embedded = false
		if err := Save(projectDir, reused); err != nil {
			return nil, err
		}
		x.NoteCached(projectID, rel)
		return reused, nil
	}

	doc := &Document{
		ContentHash: hash,
		Path:        rel,
		Duration:    duration,
		AudioHash:   audioHash,
		Words:       []Word{},
		Segments:    []Segment{},
	}
	live := x.startLiveEmbed(ctx, projectID, projectDir, doc)

	asrStarted := time.Now()
	var pending []int
	onSeg := func(seg Segment, words []Word) {
		if strings.TrimSpace(seg.Text) == "" {
			return
		}
		live.mu.Lock()
		if strings.TrimSpace(seg.ID) == "" {
			seg.ID = fmt.Sprintf("seg-%04d", len(doc.Segments))
		}
		if (doc.Language != "" && looksEnglish(doc.Language)) || isMostlyLatin(seg.Text) {
			if strings.TrimSpace(seg.TextEN) == "" {
				seg.TextEN = strings.TrimSpace(seg.Text)
			}
		}
		doc.Segments = append(doc.Segments, seg)
		idx := len(doc.Segments) - 1
		if len(words) > 0 {
			doc.Words = append(doc.Words, words...)
		}
		ready := strings.TrimSpace(doc.Segments[idx].TextEN) != ""
		if !ready {
			pending = append(pending, idx)
		}
		nseg := len(doc.Segments)
		live.mu.Unlock()
		if ready {
			live.addReady(idx)
		} else if len(pending) >= translateBatch {
			batch := pending
			pending = nil
			x.flushTranslate(ctx, doc, live, batch)
		}
		if nseg%8 == 0 {
			_ = Save(projectDir, doc)
		}
	}

	asr, err := x.runASR(ctx, wavAbs, func(at, total float64) {
		x.MarkProgress(projectID, rel, at, total)
	}, onSeg)
	if err != nil {
		_ = live.Finish()
		return nil, err
	}
	x.AddTiming(projectID, rel, TimingTranscribe, sinceMs(asrStarted))
	x.SetTimingMeta(projectID, rel, asr.Model, firstNonEmpty(asr.Device, x.asrDevice()))

	live.mu.Lock()
	if asr.Language != "" {
		doc.Language = asr.Language
	}
	if asr.Model != "" {
		doc.ASRModel = asr.Model
	}
	if len(asr.Words) > 0 && len(doc.Words) == 0 {
		doc.Words = asr.Words
	}
	if len(asr.Segments) > 0 && len(doc.Segments) == 0 {
		assignSegmentIDs(asr.Segments)
		doc.Segments = asr.Segments
		for i := range doc.Segments {
			pending = append(pending, i)
		}
	}
	live.mu.Unlock()

	if len(pending) > 0 {
		if needsEnglish(doc.Segments) {
			x.Mark(projectID, rel, StateTranslating, "")
			started := time.Now()
			x.flushTranslate(ctx, doc, live, pending)
			x.AddTiming(projectID, rel, TimingTranslate, sinceMs(started))
		} else {
			for _, i := range pending {
				live.addReady(i)
			}
		}
	}
	if err := x.ensureEnglish(ctx, doc); err != nil {
		_ = live.Finish()
		_ = Save(projectDir, doc)
		return nil, err
	}
	for i := range doc.Segments {
		if strings.TrimSpace(doc.Segments[i].TextEN) != "" {
			live.addReady(i)
		}
	}
	if err := live.Finish(); err != nil {
		x.log().Error("live transcript embed", "project", projectID, "path", rel, "err", err)
	} else if x.canEmbed() && len(doc.Segments) > 0 {
		doc.Embedded = true
	}
	if err := Save(projectDir, doc); err != nil {
		return nil, err
	}
	return doc, nil
}

func (x *Indexer) runASR(ctx context.Context, wavAbs string, progress ProgressFunc, onSeg SegmentFunc) (ASRResult, error) {
	if s, ok := x.Whisper.(StreamTranscriber); ok {
		return s.TranscribeStream(ctx, wavAbs, progress, onSeg)
	}
	asr, err := x.Whisper.Transcribe(ctx, wavAbs, progress)
	if err != nil {
		return asr, err
	}
	if onSeg != nil {
		for _, seg := range asr.Segments {
			onSeg(seg, nil)
		}
	}
	return asr, nil
}

func (x *Indexer) flushTranslate(ctx context.Context, doc *Document, live *liveEmbedder, batch []int) {
	if doc == nil || len(batch) == 0 {
		return
	}
	subset := make([]Segment, len(doc.Segments))
	copy(subset, doc.Segments)
	_ = x.ensureEnglish(ctx, &Document{Language: doc.Language, Segments: subset})
	live.mu.Lock()
	for _, i := range batch {
		if i >= 0 && i < len(doc.Segments) {
			if strings.TrimSpace(subset[i].TextEN) != "" {
				doc.Segments[i].TextEN = subset[i].TextEN
			}
		}
	}
	live.mu.Unlock()
	for _, i := range batch {
		if i >= 0 && i < len(doc.Segments) && strings.TrimSpace(doc.Segments[i].TextEN) != "" {
			live.addReady(i)
		}
	}
}

func (x *Indexer) ensureEnglish(ctx context.Context, doc *Document) error {
	if looksEnglish(doc.Language) {
		for i := range doc.Segments {
			if strings.TrimSpace(doc.Segments[i].TextEN) == "" {
				doc.Segments[i].TextEN = strings.TrimSpace(doc.Segments[i].Text)
			}
		}
	}
	if !needsEnglish(doc.Segments) {
		return nil
	}
	if x.Completer == nil {
		return fmt.Errorf("transcript translator is not configured")
	}
	completer := x.Completer()
	if completer == nil {
		return fmt.Errorf("transcript translator is not configured")
	}
	return TranslateSegments(ctx, completer, doc.Language, doc.Segments)
}

func (x *Indexer) upsert(ctx context.Context, projectID string, doc *Document) error {
	if x.Embeddings == nil || x.Qdrant == nil {
		x.log().Info("skip transcript embed", "reason", "embeddings or qdrant not configured", "path", doc.Path)
		return nil
	}
	var texts []string
	var segs []Segment
	for i, seg := range doc.Segments {
		window := NeighborWindow(doc.Segments, i)
		if strings.TrimSpace(window) == "" {
			continue
		}
		texts = append(texts, window)
		segs = append(segs, seg)
	}
	if len(texts) == 0 {
		return nil
	}
	vectors, err := x.Embeddings.Embed(ctx, texts)
	if err != nil {
		return err
	}
	if len(vectors) == 0 {
		return fmt.Errorf("embed: no vectors returned")
	}
	collection := qdrant.CollectionName(projectID)
	if err := x.Qdrant.EnsureCollection(ctx, collection, len(vectors[0])); err != nil {
		return err
	}
	if err := x.Qdrant.DeleteByPathAndKind(ctx, collection, doc.Path, KindTranscript, true); err != nil {
		return err
	}
	points := make([]qdrant.Point, 0, len(segs))
	for i, seg := range segs {
		points = append(points, qdrant.Point{
			ID:     qdrant.PointID(doc.ContentHash, seg.ID),
			Vector: vectors[i],
			Payload: map[string]any{
				"kind":         KindTranscript,
				"content_hash": doc.ContentHash,
				"path":         doc.Path,
				"start":        seg.Start,
				"end":          seg.End,
				"text":         seg.Text,
				"text_en":      seg.TextEN,
				"language":     doc.Language,
				"segment_id":   seg.ID,
			},
		})
	}
	return x.Qdrant.Upsert(ctx, collection, points)
}

// RemovePath drops Qdrant points for a deleted or replaced file.
func (x *Indexer) RemovePath(ctx context.Context, projectID, rel string) error {
	if x == nil {
		return nil
	}
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if rel == "" {
		return nil
	}
	x.Clear(projectID, rel)
	if x.Qdrant == nil {
		return nil
	}
	return x.Qdrant.DeleteByPath(ctx, qdrant.CollectionName(projectID), rel)
}

// RemoveProject drops live index state and the project's Qdrant collection.
func (x *Indexer) RemoveProject(ctx context.Context, projectID string) error {
	if x == nil {
		return nil
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil
	}
	x.clearProject(projectID)
	if x.Projects != nil {
		if project, err := x.Projects.Get(projectID); err == nil {
			_ = saveStatusFile(project.Dir, map[string]JobStatus{})
		}
	}
	if x.Qdrant == nil {
		return nil
	}
	return x.Qdrant.DeleteCollection(ctx, qdrant.CollectionName(projectID))
}

// Get loads the transcript for the current bytes of a project file.
func (x *Indexer) Get(projectID, rel string) (*Document, error) {
	if x == nil || x.Projects == nil {
		return nil, fmt.Errorf("transcripts are not configured")
	}
	project, err := x.Projects.Get(projectID)
	if err != nil {
		return nil, err
	}
	abs, err := x.Projects.ResolveFile(projectID, rel)
	if err != nil {
		return nil, err
	}
	hash, err := projects.HashFile(abs)
	if err != nil {
		return nil, err
	}
	doc, err := Load(project.Dir, hash)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("no transcript for %s", filepath.ToSlash(rel))
	}
	return doc, nil
}

// Search embeds an English query and returns matching transcript segments.
func (x *Indexer) Search(ctx context.Context, projectID, query string, paths []string, limit int) ([]qdrant.Hit, error) {
	return x.search(ctx, projectID, query, paths, "", []string{KindImage, KindVideoScene, KindGeneratedAudio}, limit)
}

// SearchGeneratedAudio searches prompts, lyrics, voice characteristics, and
// descriptions stored for generated audio assets.
func (x *Indexer) SearchGeneratedAudio(ctx context.Context, projectID, query string, paths []string, limit int) ([]qdrant.Hit, error) {
	return x.search(ctx, projectID, query, paths, KindGeneratedAudio, nil, limit)
}

// SearchAll embeds an English query and returns stills, scenes, and speech hits.
func (x *Indexer) SearchAll(ctx context.Context, projectID, query string, limit int) ([]qdrant.Hit, error) {
	if limit < 1 {
		limit = 24
	}
	return x.search(ctx, projectID, query, nil, "", nil, limit)
}

func (x *Indexer) search(ctx context.Context, projectID, query string, paths []string, kind string, excludeKinds []string, limit int) ([]qdrant.Hit, error) {
	if x == nil || x.Embeddings == nil || x.Qdrant == nil {
		switch {
		case kind == KindImage:
			return nil, fmt.Errorf("image search is not configured")
		case kind == KindVideoScene:
			return nil, fmt.Errorf("scene search is not configured")
		case kind == "" && len(excludeKinds) == 0:
			return nil, fmt.Errorf("media search is not configured")
		default:
			return nil, fmt.Errorf("transcript search is not configured")
		}
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	vecs, err := x.Embeddings.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("embed: empty query vector")
	}
	return x.Qdrant.Search(ctx, qdrant.CollectionName(projectID), vecs[0], qdrant.SearchOpts{
		Paths:        paths,
		Kind:         kind,
		ExcludeKinds: excludeKinds,
		Limit:        limit,
	})
}

func assignSegmentIDs(segments []Segment) {
	for i := range segments {
		if strings.TrimSpace(segments[i].ID) == "" {
			segments[i].ID = fmt.Sprintf("seg-%04d", i)
		}
	}
}

func (x *Indexer) asrDevice() string {
	if x == nil {
		return ""
	}
	if w, ok := x.Whisper.(*FasterWhisper); ok {
		return strings.TrimSpace(w.Device)
	}
	return ""
}

func (x *Indexer) logTimings(projectID, rel string) {
	if x == nil {
		return
	}
	st := x.lookup(projectID, rel)
	t := st.Timings
	x.log().Info("index timings",
		"project", projectID,
		"path", rel,
		"state", st.State,
		"upload_ms", t.UploadMs,
		"queue_ms", t.QueueMs,
		"extract_ms", t.ExtractMs,
		"transcribe_ms", t.TranscribeMs,
		"translate_ms", t.TranslateMs,
		"describe_ms", t.DescribeMs,
		"index_ms", t.IndexMs,
		"total_ms", t.TotalMs,
		"cached", t.Cached,
		"model", t.Model,
		"device", t.Device,
	)
}

// HasSpeech is true for video/audio extensions that may contain a soundtrack.
func HasSpeech(rel string) bool {
	return hasAudioExt(rel)
}

func hasAudioExt(rel string) bool {
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".mp4", ".mov", ".mkv", ".webm", ".avi", ".m4v", ".ts", ".mts",
		".mp3", ".wav", ".aac", ".flac", ".m4a", ".ogg", ".opus":
		return true
	default:
		return false
	}
}
