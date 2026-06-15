package mailbox

import (
	"context"
	"errors"
	"log/slog"
	"strings"
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
	Svc       *Service     // optional — required for auto-issue (Phase 7)
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
	// Surface filter count up-front so the operator immediately knows
	// whether the "ingested=0" outcome is "no filters yet" vs "filters
	// exist but nothing matched".
	filters, ferr := w.Repo.cachedFilters(ctx)
	filterCount := len(filters)
	if ferr != nil {
		w.Log.Warn("mailbox: filter cache load failed at cycle begin", "err", ferr)
	}
	w.Log.Info("mailbox: poll cycle begin",
		"host", w.Cfg.Host,
		"user", w.Cfg.Username,
		"folders", len(folders),
		"folder_names", folders,
		"filters", filterCount,
	)
	if filterCount == 0 {
		w.Log.Warn("mailbox: no community_mail_filter rows — every email will be skipped. Add filters via /c/<slug>/admin/mail-filters")
	}

	var (
		ingested  int
		fetched   int
		matched   int
		skippedNF int // skipped: no filter
	)
	for _, name := range folders {
		if ctx.Err() != nil {
			return
		}
		stats, err := w.scanFolder(ctx, c, name)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			w.Log.Warn("mailbox: scan folder failed", "folder", name, "err", err)
			continue
		}
		ingested += stats.inserted
		fetched += stats.fetched
		matched += stats.matched
		skippedNF += stats.skippedNoFilter
	}
	w.Log.Info("mailbox: poll cycle end",
		"dur_ms", time.Since(start).Milliseconds(),
		"fetched", fetched,
		"matched", matched,
		"skipped_no_filter", skippedNF,
		"ingested", ingested,
	)
}

// scanStats is the per-folder summary returned to the cycle aggregator
// so the cycle-end log reports complete numbers.
type scanStats struct {
	fetched         int
	matched         int
	skippedNoFilter int
	inserted        int
}

// scanFolder examines one folder, fetches new envelopes, runs each
// through MatchFrom and persists the matches. Returns the per-folder
// stat block so the cycle aggregator can log totals at the end.
func (w *PollWorker) scanFolder(ctx context.Context, c *imapClient, name string) (scanStats, error) {
	var stats scanStats
	info, err := c.examineReadOnly(name)
	if err != nil {
		return stats, err
	}
	folder, err := w.Repo.UpsertFolder(ctx, w.AccountID, name, info.UIDValidity)
	if err != nil {
		return stats, err
	}
	w.Log.Info("mailbox: folder examined",
		"folder", name,
		"messages", info.NumMessages,
		"uidvalidity", info.UIDValidity,
		"uidnext", info.UIDNext,
		"last_uid_cursor", folder.LastUID,
	)
	if info.NumMessages == 0 {
		return stats, nil
	}
	if folder.LastUID >= info.UIDNext-1 && info.UIDNext > 0 {
		w.Log.Info("mailbox: folder up-to-date", "folder", name, "last_uid", folder.LastUID)
		return stats, nil
	}
	envs, err := c.fetchEnvelopesSince(folder.LastUID)
	if err != nil {
		return stats, err
	}
	stats.fetched = len(envs)
	w.Log.Info("mailbox: folder fetched",
		"folder", name,
		"fetched", stats.fetched,
		"since_uid", folder.LastUID,
	)
	if len(envs) == 0 {
		return stats, nil
	}

	var maxUID uint32 = folder.LastUID
	for _, e := range envs {
		if e.UID > maxUID {
			maxUID = e.UID
		}
		filter, matched, err := MatchFrom(ctx, w.Repo, e.FromAddr)
		if err != nil {
			w.Log.Warn("mailbox: match failed", "folder", name, "uid", e.UID, "err", err)
			continue
		}
		if matched {
			stats.matched++
		} else {
			stats.skippedNoFilter++ // metric name kept; actually "unassigned"
		}
		// Pull the text body alongside the envelope so /inbox search has
		// content to index. Failure here logs but doesn't block the
		// ingest — we still want the row + attachment metadata.
		bodyText := ""
		if _, text, html, err := c.fetchEnvelopeWithBody(e.UID); err == nil {
			bodyText = strings.TrimSpace(text)
			if bodyText == "" {
				bodyText = ExtractIssueBody("", html)
			}
		} else {
			w.Log.Warn("mailbox: body fetch failed (search index will lack body)",
				"folder", name, "uid", e.UID, "err", err)
		}

		ingest := IngestInsert{
			FolderID:    folder.ID,
			UID:         e.UID,
			UIDValidity: info.UIDValidity,
			MessageID:   e.MessageID,
			FromAddr:    e.FromAddr,
			FromName:    e.FromName,
			Subject:     e.Subject,
			BodyText:    bodyText,
			ReceivedAt:  e.InternalDate,
		}
		if matched {
			ingest.CommunityID = filter.CommunityID
			ingest.MatchedFilterID = filter.ID
		}
		ingestID, isNew, err := w.Repo.InsertIngest(ctx, ingest)
		if err != nil {
			w.Log.Warn("mailbox: ingest insert failed",
				"folder", name, "uid", e.UID, "err", err)
			continue
		}
		if !isNew {
			w.Log.Debug("mailbox: duplicate ingest skipped",
				"folder", name, "uid", e.UID, "from", e.FromAddr)
			continue
		}
		stats.inserted++
		if err := w.Repo.InsertAttachments(ctx, ingestID, e.Attachments); err != nil {
			w.Log.Warn("mailbox: attachments index failed",
				"folder", name, "uid", e.UID, "err", err)
		}
		if matched && filter.ToIssue && w.Svc != nil {
			if err := w.autoCreateIssueFor(ctx, c, ingestID, filter.CommunityID, e); err != nil {
				w.Log.Warn("mailbox: auto-issue failed",
					"folder", name, "uid", e.UID, "err", err)
			}
		}
		// Broadcast to the destination community when matched, or to every
		// admin's stream when unassigned (poll loop has no specific
		// community to ping; broadcast to all admin communities).
		if matched {
			w.broadcast(filter.CommunityID)
		} else {
			w.broadcast(UnassignedCommunityID)
		}
		communityForLog := filter.CommunityID
		if !matched {
			communityForLog = "(unassigned)"
		}
		w.Log.Info("mailbox: ingested",
			"folder", name,
			"uid", e.UID,
			"from", e.FromAddr,
			"community", communityForLog,
			"to_issue", matched && filter.ToIssue,
			"attachments", len(e.Attachments),
		)
	}
	if maxUID > folder.LastUID {
		if err := w.Repo.SetFolderLastUID(ctx, folder.ID, maxUID); err != nil {
			w.Log.Warn("mailbox: cursor advance failed",
				"folder", name, "want", maxUID, "err", err)
		}
	}
	w.Log.Info("mailbox: folder summary",
		"folder", name,
		"fetched", stats.fetched,
		"matched", stats.matched,
		"skipped_no_filter", stats.skippedNoFilter,
		"ingested", stats.inserted,
		"cursor_to", maxUID,
	)
	return stats, nil
}

// autoCreateIssueFor is called only for to_issue filter matches.
// Re-uses the already-authenticated session to fetch the text bodies
// (one extra UID FETCH for the BODY.PEEK[<text part>]) and then
// delegates to Service.AutoCreateIssue. Errors here do NOT roll back
// the ingest row — the email is still queued for manual attention.
func (w *PollWorker) autoCreateIssueFor(ctx context.Context, c *imapClient, ingestID, communityID string, e FetchedEnvelope) error {
	_, text, html, err := c.fetchEnvelopeWithBody(e.UID)
	if err != nil {
		return err
	}
	if _, err := w.Svc.AutoCreateIssue(ctx, AutoCreateIssueInput{
		IngestID:    ingestID,
		CommunityID: communityID,
		Subject:     e.Subject,
		TextBody:    text,
		HTMLBody:    html,
	}); err != nil {
		return err
	}
	return nil
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
