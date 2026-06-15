package mailbox

import (
	"context"
	"errors"
	"log/slog"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/atvirokodosprendimai/forumchat/internal/natsx"
)

// PollWorker dials the configured IMAP account on a ticker, walks every
// folder, and ingests new messages whose From: matches a per-community
// filter. Phase 3 onward persists matched envelopes; non-matches are
// silently skipped.
//
// The worker is single-instance per process. Multi-process coordination
// would require a leader-election lock; v1 ships one binary.
type PollWorker struct {
	Cfg       AccountConfig
	AccountID string // mailbox_account.id resolved by Repo.EnsureAccount
	Interval  time.Duration
	Repo      *Repo
	Bus       *Bus         // optional — nil disables in-process fan-out
	NATS      *natsgo.Conn // optional — nil disables cross-process fan-out
	Log       *slog.Logger
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
// envelopes greater than the persisted per-folder last_uid, match each
// from-address against community_mail_filter, persist matched rows.
func (w *PollWorker) cycle(ctx context.Context) {
	if w.Cfg.Host == "" || w.Cfg.Username == "" {
		w.Log.Warn("mailbox: poll cycle skipped — host/user not configured")
		return
	}
	if w.Repo == nil || w.AccountID == "" {
		w.Log.Warn("mailbox: poll cycle skipped — repo/account not wired")
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

	var ingested int
	for _, name := range folders {
		if ctx.Err() != nil {
			return
		}
		n, err := w.scanFolder(ctx, c, name)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			w.Log.Warn("mailbox: scan folder failed", "folder", name, "err", err)
			continue
		}
		ingested += n
	}
	w.Log.Info("mailbox: poll cycle end",
		"dur_ms", time.Since(start).Milliseconds(),
		"ingested", ingested,
	)
}

// scanFolder examines one folder, fetches new envelopes, runs each
// through MatchFrom and persists the matches. Returns the count of
// new email_ingest rows the cycle produced (excluding duplicates).
func (w *PollWorker) scanFolder(ctx context.Context, c *imapClient, name string) (int, error) {
	info, err := c.examineReadOnly(name)
	if err != nil {
		return 0, err
	}
	folder, err := w.Repo.UpsertFolder(ctx, w.AccountID, name, info.UIDValidity)
	if err != nil {
		return 0, err
	}
	if info.NumMessages == 0 {
		return 0, nil
	}
	envs, err := c.fetchEnvelopesSince(folder.LastUID)
	if err != nil {
		return 0, err
	}
	if len(envs) == 0 {
		return 0, nil
	}

	var maxUID uint32 = folder.LastUID
	var inserted int
	for _, e := range envs {
		if e.UID > maxUID {
			maxUID = e.UID
		}
		filter, ok, err := MatchFrom(ctx, w.Repo, e.FromAddr)
		if err != nil {
			w.Log.Warn("mailbox: match failed", "folder", name, "uid", e.UID, "err", err)
			continue
		}
		if !ok {
			continue
		}
		ingestID, isNew, err := w.Repo.InsertIngest(ctx, IngestInsert{
			FolderID:        folder.ID,
			UID:             e.UID,
			UIDValidity:     info.UIDValidity,
			MessageID:       e.MessageID,
			FromAddr:        e.FromAddr,
			FromName:        e.FromName,
			Subject:         e.Subject,
			ReceivedAt:      e.InternalDate,
			CommunityID:     filter.CommunityID,
			MatchedFilterID: filter.ID,
		})
		if err != nil {
			w.Log.Warn("mailbox: ingest insert failed",
				"folder", name, "uid", e.UID, "err", err)
			continue
		}
		if !isNew {
			continue
		}
		inserted++
		if err := w.Repo.InsertAttachments(ctx, ingestID, e.Attachments); err != nil {
			w.Log.Warn("mailbox: attachments index failed",
				"folder", name, "uid", e.UID, "err", err)
		}
		w.broadcast(filter.CommunityID)
		w.Log.Info("mailbox: ingested",
			"folder", name,
			"uid", e.UID,
			"from", e.FromAddr,
			"community", filter.CommunityID,
			"to_issue", filter.ToIssue,
			"attachments", len(e.Attachments),
		)
	}
	if maxUID > folder.LastUID {
		if err := w.Repo.SetFolderLastUID(ctx, folder.ID, maxUID); err != nil {
			w.Log.Warn("mailbox: cursor advance failed",
				"folder", name, "want", maxUID, "err", err)
		}
	}
	return inserted, nil
}

// broadcast fires both the in-process Bus and the NATS subject for the
// community so every viewer's inbox SSE wakes and re-renders.
func (w *PollWorker) broadcast(communityID string) {
	if w.Bus != nil {
		w.Bus.Broadcast(communityID)
	}
	if w.NATS != nil && w.NATS.IsConnected() {
		_ = w.NATS.Publish(natsx.MailboxSubject(communityID), []byte(communityID))
	}
}
