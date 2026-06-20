package rag

import (
	"context"
	"log/slog"
	"time"
)

// Worker drains embed_outbox on an interval, embedding changed rows and updating
// the vector store. It is the RAG counterpart to the search_fts triggers: those
// keep the FTS index live synchronously; this keeps the vector index live
// asynchronously (embedding can't run inside a trigger). Realtime "catchup" is
// therefore eventually-consistent — bounded by Interval.
type Worker struct {
	Repo     *Repo
	Svc      *Service
	Interval time.Duration
	Batch    int
	Log      *slog.Logger
}

// Start runs the drain loop until ctx is cancelled. Call once at boot.
func (w *Worker) Start(ctx context.Context) {
	if w.Interval <= 0 {
		w.Interval = 10 * time.Second
	}
	if w.Batch <= 0 {
		w.Batch = 64
	}
	go func() {
		// Brief initial delay so boot migrations / first requests aren't
		// contending with a large backfill drain.
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
		w.tick(ctx)
		t := time.NewTicker(w.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				w.tick(ctx)
			}
		}
	}()
}

// tick drains the queue in batches until it is empty or an error occurs. On the
// first process error it stops and returns, leaving the job UNacked so the next
// tick retries — this is deliberate backpressure: the dominant failure is the
// embedder (Ollama) being unreachable, and retrying the whole queue next
// interval is the right response. (Trade-off: a genuinely poison row would block
// the queue head; content here is plain text so that's unlikely. A reindex
// clears any stuck state.)
func (w *Worker) tick(ctx context.Context) {
	if w.Svc == nil || w.Repo == nil {
		return
	}
	processed := 0
	for {
		if ctx.Err() != nil {
			return
		}
		items, err := w.Repo.Dequeue(ctx, w.Batch)
		if err != nil {
			w.Log.Warn("rag: dequeue", "err", err)
			return
		}
		if len(items) == 0 {
			if processed > 0 {
				w.Log.Info("rag: indexed", "count", processed)
			}
			return
		}
		for _, it := range items {
			if ctx.Err() != nil {
				return
			}
			if err := w.Svc.process(ctx, it); err != nil {
				w.Log.Warn("rag: process (will retry)", "kind", it.Kind, "ref", it.RefID, "op", it.Op, "err", err)
				if processed > 0 {
					w.Log.Info("rag: indexed", "count", processed)
				}
				return
			}
			if err := w.Repo.Ack(ctx, it.Seq); err != nil {
				w.Log.Warn("rag: ack", "seq", it.Seq, "err", err)
				return
			}
			processed++
		}
	}
}
