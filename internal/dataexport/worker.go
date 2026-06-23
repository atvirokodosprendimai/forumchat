package dataexport

import (
	"context"
	"log/slog"
	"os"
	"time"
)

// Worker is the background queue behind data exports. One goroutine drains
// pending requests (building one ZIP at a time — exports are I/O heavy and there
// is no value in parallel builds), and a second expires artifacts past their TTL
// (deletes the ZIP, marks the row expired). Mirrors uploads.SweepWorker.
type Worker struct {
	Svc           *Service
	PollInterval  time.Duration // pending-queue cadence; 15s default
	SweepInterval time.Duration // expiry-sweep cadence; 1h default
	Log           *slog.Logger
}

// Start runs until ctx is cancelled. Safe to call once at boot.
func (w *Worker) Start(ctx context.Context) {
	if w.PollInterval <= 0 {
		w.PollInterval = 15 * time.Second
	}
	if w.SweepInterval <= 0 {
		w.SweepInterval = time.Hour
	}
	go w.drainLoop(ctx)
	go w.sweepLoop(ctx)
}

func (w *Worker) drainLoop(ctx context.Context) {
	t := time.NewTicker(w.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.drain(ctx)
		}
	}
}

// drain builds every queued export, one at a time, until the queue is empty.
func (w *Worker) drain(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		e, ok, err := w.Svc.Repo.NextPending(ctx)
		if err != nil {
			w.log().Warn("dataexport: claim pending", "err", err)
			return
		}
		if !ok {
			return
		}
		w.log().Info("dataexport: building", "export", e.ID, "community", e.CommunityID)
		if err := w.Svc.Build(ctx, e); err != nil {
			// Build already marked the row failed; just log and move on.
			w.log().Warn("dataexport: build", "export", e.ID, "err", err)
			continue
		}
		w.log().Info("dataexport: ready", "export", e.ID, "community", e.CommunityID)
	}
}

func (w *Worker) sweepLoop(ctx context.Context) {
	// Run once shortly after boot so a server that was down past an expiry still
	// cleans up promptly, then on the regular cadence.
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Minute):
	}
	w.sweep(ctx)
	t := time.NewTicker(w.SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.sweep(ctx)
		}
	}
}

// sweep deletes the ZIP of every export past its expiry and marks it expired —
// after which a fresh request is required to download again.
func (w *Worker) sweep(ctx context.Context) {
	victims, err := w.Svc.Repo.ListExpirable(ctx, time.Now())
	if err != nil {
		w.log().Warn("dataexport: list expirable", "err", err)
		return
	}
	for _, e := range victims {
		if e.RelPath != "" {
			if err := os.Remove(w.Svc.ZipPath(e)); err != nil && !os.IsNotExist(err) {
				w.log().Warn("dataexport: remove expired zip", "export", e.ID, "err", err)
			}
		}
		if err := w.Svc.Repo.MarkExpired(ctx, e.ID); err != nil {
			w.log().Warn("dataexport: mark expired", "export", e.ID, "err", err)
		}
	}
	if len(victims) > 0 {
		w.log().Info("dataexport: swept expired exports", "count", len(victims))
	}
}

func (w *Worker) log() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}
