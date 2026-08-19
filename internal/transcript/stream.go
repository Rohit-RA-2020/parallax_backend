package transcript

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"parallax/internal/qdrant"
)

const (
	liveEmbedFlushN = 8
	liveEmbedWait   = 400 * time.Millisecond
)

type liveEmbedder struct {
	x          *Indexer
	ctx        context.Context
	projectID  string
	projectDir string
	doc        *Document

	mu        sync.Mutex
	ready     []int
	seen      map[int]bool
	cleared   bool
	ch        chan struct{}
	stop      chan struct{}
	done      chan struct{}
	err       error
	enabled   bool
	indexTime time.Duration
}

func (x *Indexer) startLiveEmbed(ctx context.Context, projectID, projectDir string, doc *Document) *liveEmbedder {
	l := &liveEmbedder{
		x:          x,
		ctx:        ctx,
		projectID:  projectID,
		projectDir: projectDir,
		doc:        doc,
		seen:       map[int]bool{},
		ch:         make(chan struct{}, 1),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		enabled:    x.canEmbed(),
	}
	if l.enabled {
		go l.loop()
	} else {
		close(l.done)
	}
	return l
}

func (l *liveEmbedder) addReady(i int) {
	if l == nil || i < 0 {
		return
	}
	l.mu.Lock()
	if l.seen[i] {
		l.mu.Unlock()
		return
	}
	l.seen[i] = true
	l.ready = append(l.ready, i)
	n := len(l.ready)
	l.mu.Unlock()
	if !l.enabled {
		return
	}
	if n >= liveEmbedFlushN {
		l.kick()
	} else {
		l.kickSoft()
	}
}

func (l *liveEmbedder) kick() {
	select {
	case l.ch <- struct{}{}:
	default:
	}
}

func (l *liveEmbedder) kickSoft() {
	select {
	case l.ch <- struct{}{}:
	default:
	}
}

func (l *liveEmbedder) loop() {
	defer close(l.done)
	tick := time.NewTicker(liveEmbedWait)
	defer tick.Stop()
	for {
		select {
		case <-l.stop:
			l.flush()
			return
		case <-l.ctx.Done():
			l.flush()
			return
		case <-l.ch:
			l.flush()
		case <-tick.C:
			l.mu.Lock()
			n := len(l.ready)
			l.mu.Unlock()
			if n > 0 {
				l.flush()
			}
		}
	}
}

func (l *liveEmbedder) Finish() error {
	if l == nil {
		return nil
	}
	if l.enabled {
		select {
		case <-l.stop:
		default:
			close(l.stop)
		}
		<-l.done
	}
	if l.enabled && l.err == nil && l.indexTime > 0 {
		l.x.AddTiming(l.projectID, l.doc.Path, TimingIndex, l.indexTime.Milliseconds())
	}
	return l.err
}

func (l *liveEmbedder) flush() {
	if l == nil || !l.enabled || l.x == nil || l.doc == nil {
		return
	}
	l.mu.Lock()
	batch := append([]int(nil), l.ready...)
	l.ready = l.ready[:0]
	segs := append([]Segment(nil), l.doc.Segments...)
	l.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	started := time.Now()
	if err := l.x.upsertSegmentIndexes(l.ctx, l.projectID, l.doc, segs, batch, !l.cleared); err != nil {
		l.mu.Lock()
		if l.err == nil {
			l.err = err
		}
		l.ready = append(batch, l.ready...)
		for _, i := range batch {
			delete(l.seen, i)
		}
		l.mu.Unlock()
		return
	}
	l.cleared = true
	l.indexTime += time.Since(started)
}

func (x *Indexer) upsertSegmentIndexes(ctx context.Context, projectID string, doc *Document, segs []Segment, indexes []int, clear bool) error {
	if x.Embeddings == nil || x.Qdrant == nil || doc == nil {
		return nil
	}
	var texts []string
	var chosen []Segment
	for _, i := range indexes {
		if i < 0 || i >= len(segs) {
			continue
		}
		window := NeighborWindow(segs, i)
		if strings.TrimSpace(window) == "" {
			continue
		}
		texts = append(texts, window)
		chosen = append(chosen, segs[i])
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
	if clear {
		if err := x.Qdrant.DeleteByPathAndKind(ctx, collection, doc.Path, KindTranscript, true); err != nil {
			return err
		}
	}
	points := make([]qdrant.Point, 0, len(chosen))
	for i, seg := range chosen {
		id := seg.ID
		if strings.TrimSpace(id) == "" {
			id = fmt.Sprintf("seg-%04d", i)
		}
		points = append(points, qdrant.Point{
			ID:     qdrant.PointID(doc.ContentHash, id),
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
				"segment_id":   id,
			},
		})
	}
	return x.Qdrant.Upsert(ctx, collection, points)
}
