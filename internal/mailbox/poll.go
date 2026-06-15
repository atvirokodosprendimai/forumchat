package mailbox

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// PollWorker dials the configured IMAP account on a ticker, walks every
// folder, and (in later phases) ingests new messages whose From: matches
// a per-community filter. Phase 2 stops at logging — no DB rows are
// written from the worker yet.
//
// The worker is single-instance per process. Multi-process coordination
// would require a leader-election lock; v1 ships one binary.
type PollWorker struct {
	Cfg      AccountConfig
	Interval time.Duration
	Log      *slog.Logger
}

// Start spawns the poll goroutine. It returns immediately. The worker
// stops when ctx is cancelled.
func (w *PollWorker) Start(ctx context.Context) {
	if w.Interval <= 0 {
		w.Interval = 2 * time.Minute
	}
	go w.run(ctx)
}

func (w *PollWorker) run(ctx context.Context) {
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	// Fire one cycle immediately so a freshly-booted process reports
	// success/failure without waiting a full interval. Then settle into
	// the ticker cadence.
	w.cycle(ctx)
	for {
		select {
		case <-ctx.Done():
			w.Log.Info("mailbox: poll worker stopping")
			return
		case <-t.C:
			w.cycle(ctx)
		}
	}
}

// cycle is one poll pass: dial, list folders, examine each, fetch
// envelopes greater than the cached last_uid (Phase 2 holds last_uid in
// memory; Phase 3 persists it). Logs results, never writes rows.
func (w *PollWorker) cycle(ctx context.Context) {
	if w.Cfg.Host == "" || w.Cfg.Username == "" {
		w.Log.Warn("mailbox: poll cycle skipped — host/user not configured")
		return
	}
	start := time.Now()
	c, err := dial(w.Cfg)
	if err != nil {
		w.Log.Error("mailbox: dial failed", "err", err)
		return
	}
	defer c.close()

	folders, err := c.listFolders()
	if err != nil {
		w.Log.Error("mailbox: list folders failed", "err", err)
		return
	}
	w.Log.Info("mailbox: poll cycle begin", "folders", len(folders))

	for _, name := range folders {
		if ctx.Err() != nil {
			return
		}
		if err := w.scanFolder(c, name); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			w.Log.Warn("mailbox: scan folder failed", "folder", name, "err", err)
		}
	}
	w.Log.Info("mailbox: poll cycle end", "dur_ms", time.Since(start).Milliseconds())
}

func (w *PollWorker) scanFolder(c *imapClient, name string) error {
	info, err := c.examineReadOnly(name)
	if err != nil {
		return err
	}
	if info.NumMessages == 0 {
		w.Log.Info("mailbox: folder empty", "folder", name, "uidvalidity", info.UIDValidity)
		return nil
	}
	// Phase 2: no persisted cursor yet — log everything in the folder
	// since UID 0 would mean "every message ever", which is fine for
	// the read-only proof but noisy on big mailboxes. Use UIDNext-1 as
	// a reasonable upper bound on "what we'd consider new" and fetch
	// only the envelope so we never download bodies.
	since := uint32(0)
	if info.UIDNext > 1 {
		since = info.UIDNext - 2 // last 1 message — Phase 2 sanity check
	}
	envs, err := c.fetchEnvelopesSince(since)
	if err != nil {
		return err
	}
	w.Log.Info("mailbox: folder scanned",
		"folder", name,
		"uidvalidity", info.UIDValidity,
		"uidnext", info.UIDNext,
		"total", info.NumMessages,
		"sample", len(envs),
	)
	for _, e := range envs {
		w.Log.Info("mailbox: envelope",
			"folder", name,
			"uid", e.UID,
			"from", e.FromAddr,
			"subject", e.Subject,
			"date", e.InternalDate.Format(time.RFC3339),
		)
	}
	return nil
}
