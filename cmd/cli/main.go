package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/config"
	"github.com/atvirokodosprendimai/forumchat/internal/mailbox"
	"github.com/atvirokodosprendimai/forumchat/internal/projects"
	"github.com/atvirokodosprendimai/forumchat/internal/rag"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `forumchat-cli — administrative commands

usage:
  forumchat-cli invite [count] [max-uses]       create N invite codes (default 1); max-uses optional, omit for unlimited
  forumchat-cli role <email> <member|moderator|admin>
  forumchat-cli ban <email> [duration] [cleanup]
        ban member; duration like 24h or "-" for permanent
        cleanup: comma-separated subset of chat,threads,posts or "all"
        e.g. forumchat-cli ban abuser@example.com - all
  forumchat-cli unban <email>
  forumchat-cli approve <email>             approve a single pending membership
  forumchat-cli approve-all                 approve every pending join request in the bootstrap community
  forumchat-cli mailbox rescan              reset every IMAP folder cursor to UID 0; next poll re-fetches everything
  forumchat-cli mailbox wipe                delete every email_ingest + email_ingest_attachment + email_ingest_fts row
                                            and reset folder cursors — full cold start
  forumchat-cli mailbox reprocess-filter <filter-id>
                                            walk every email_ingest row matching the filter, call AutoCreateIssue
                                            per row (idempotent via email_ingest_issue). Use this when you add a
                                            to_issue=true filter and want past matches turned into issues too.
  forumchat-cli project mv-attachment <attachment-id> <to-project-id>
                                            move one project_attachments row to a different project. File bytes
                                            stay in uploads (deduped by SHA-256); only the project pointer moves.
  forumchat-cli reindex [slug|all]          re-queue community content for RAG embedding (default all). The
                                            running server's worker drains it. For a clean rebuild / backend
                                            switch: stop server, delete RAG_STORE_PATH, reindex, start server.
`)
}

func parseCleanup(s string) []string {
	var out []string
	cur := ""
	for _, ch := range s {
		if ch == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(ch)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func run() error {
	if len(os.Args) < 2 {
		usage()
		return errors.New("missing command")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := config.NewLogger(cfg)
	ctx := context.Background()
	db, err := sqlite.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := sqlite.Migrate(ctx, db); err != nil {
		return err
	}

	cRepo := community.NewRepo(db)
	c, err := cRepo.BootstrapOrFetch(ctx, cfg.CommunitySlug, cfg.CommunityName)
	if err != nil {
		return fmt.Errorf("bootstrap community: %w", err)
	}

	aRepo := auth.NewRepo(db)
	svc := &auth.Service{
		Repo:      aRepo,
		Mailer:    &auth.LogMailer{Log: log},
		BaseURL:   cfg.BaseURL,
		VerifyTTL: 48 * time.Hour,
		InviteTTL: 30 * 24 * time.Hour,
	}

	switch os.Args[1] {
	case "invite":
		n := 1
		var maxUses *int
		if len(os.Args) >= 3 {
			fmt.Sscanf(os.Args[2], "%d", &n)
			if n < 1 {
				n = 1
			}
		}
		// Optional third arg = max-uses per code. Default unlimited (Discord-style).
		if len(os.Args) >= 4 {
			v := 0
			if _, err := fmt.Sscanf(os.Args[3], "%d", &v); err == nil && v > 0 {
				maxUses = &v
			}
		}
		for i := 0; i < n; i++ {
			code, err := svc.IssueInvite(ctx, c.ID, nil, maxUses)
			if err != nil {
				return err
			}
			fmt.Println(code)
		}
	case "role":
		if len(os.Args) < 4 {
			usage()
			return errors.New("usage: role <email> <role>")
		}
		email, role := os.Args[2], auth.Role(os.Args[3])
		if role != auth.RoleMember && role != auth.RoleMod && role != auth.RoleAdmin {
			return fmt.Errorf("unknown role: %s", role)
		}
		u, err := aRepo.UserByEmail(ctx, email)
		if err != nil {
			return err
		}
		m, err := aRepo.MembershipFor(ctx, u.ID, c.ID)
		if err != nil {
			return err
		}
		if err := aRepo.UpdateMembershipRole(ctx, m.ID, role); err != nil {
			return err
		}
		fmt.Printf("updated %s -> %s\n", email, role)
	case "ban":
		if len(os.Args) < 3 {
			usage()
			return errors.New("usage: ban <email> [duration] [cleanup]")
		}
		email := os.Args[2]
		var until *time.Time
		if len(os.Args) >= 4 && os.Args[3] != "" && os.Args[3] != "-" {
			d, err := time.ParseDuration(os.Args[3])
			if err != nil {
				return err
			}
			t := time.Now().Add(d)
			until = &t
		} else {
			t := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
			until = &t
		}
		// Optional fourth arg: comma-separated cleanup list.
		// e.g. "chat,threads,posts" or "all" to wipe everything.
		var opts auth.CleanupOptions
		if len(os.Args) >= 5 {
			for _, k := range parseCleanup(os.Args[4]) {
				switch k {
				case "chat":
					opts.Chat = true
				case "threads":
					opts.Threads = true
				case "posts":
					opts.Posts = true
				case "all":
					opts.Chat, opts.Threads, opts.Posts = true, true, true
				}
			}
		}
		u, err := aRepo.UserByEmail(ctx, email)
		if err != nil {
			return err
		}
		m, err := aRepo.MembershipFor(ctx, u.ID, c.ID)
		if err != nil {
			return err
		}
		if err := aRepo.UpdateBan(ctx, m.ID, until); err != nil {
			return err
		}
		if opts.Chat || opts.Threads || opts.Posts {
			if err := aRepo.CleanupUserContent(ctx, u.ID, c.ID, opts); err != nil {
				return fmt.Errorf("cleanup: %w", err)
			}
		}
		fmt.Printf("banned %s until %s (cleanup: chat=%v threads=%v posts=%v)\n",
			email, until.Format(time.RFC3339), opts.Chat, opts.Threads, opts.Posts)
	case "unban":
		if len(os.Args) < 3 {
			return errors.New("usage: unban <email>")
		}
		email := os.Args[2]
		u, err := aRepo.UserByEmail(ctx, email)
		if err != nil {
			return err
		}
		m, err := aRepo.MembershipFor(ctx, u.ID, c.ID)
		if err != nil {
			return err
		}
		if err := aRepo.UpdateBan(ctx, m.ID, nil); err != nil {
			return err
		}
		fmt.Printf("unbanned %s\n", email)
	case "approve":
		if len(os.Args) < 3 {
			return errors.New("usage: approve <email>")
		}
		email := os.Args[2]
		u, err := aRepo.UserByEmail(ctx, email)
		if err != nil {
			return err
		}
		m, err := aRepo.MembershipFor(ctx, u.ID, c.ID)
		if err != nil {
			return err
		}
		if err := aRepo.ApproveMembership(ctx, m.ID); err != nil {
			if errors.Is(err, auth.ErrNotFound) {
				fmt.Printf("%s already approved\n", email)
				return nil
			}
			return err
		}
		fmt.Printf("approved %s\n", email)
	case "approve-all":
		pending, err := aRepo.ListPendingMemberships(ctx, c.ID)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			fmt.Println("no pending requests")
			return nil
		}
		n := 0
		for _, m := range pending {
			if err := aRepo.ApproveMembership(ctx, m.ID); err != nil {
				if errors.Is(err, auth.ErrNotFound) {
					continue
				}
				fmt.Fprintf(os.Stderr, "approve %s: %v\n", m.Email, err)
				continue
			}
			fmt.Printf("approved %s\n", m.Email)
			n++
		}
		fmt.Printf("done — approved %d of %d\n", n, len(pending))
	case "mailbox":
		if len(os.Args) < 3 {
			usage()
			return errors.New("usage: mailbox <rescan|wipe|prune-skipped|reprocess-filter|apply-filter|decode-bodies|rerender-upload-bodies>")
		}
		mRepo := mailbox.NewRepo(db)
		acc, err := mRepo.EnsureAccount(ctx, mailbox.AccountConfig{
			Host:     cfg.MailboxHost,
			Port:     cfg.MailboxPort,
			Username: cfg.MailboxUser,
			Password: cfg.MailboxPass,
			TLSMode:  cfg.MailboxTLS,
		})
		if err != nil {
			return fmt.Errorf("mailbox: no account configured (%w)", err)
		}
		switch os.Args[2] {
		case "rescan":
			n, err := mRepo.ResetAllFolderCursors(ctx, acc.ID)
			if err != nil {
				return err
			}
			fmt.Printf("reset %d folder cursors — next poll cycle will re-fetch everything\n", n)
		case "wipe":
			n, err := mRepo.WipeIngest(ctx, acc.ID)
			if err != nil {
				return err
			}
			fmt.Printf("wiped %d ingest rows + reset folder cursors — full cold start\n", n)
		case "rerender-upload-bodies":
			projsRepo := projects.NewRepo(db)
			rows, err := projsRepo.AllIssueBodies(ctx)
			if err != nil {
				return err
			}
			fixed := 0
			now := time.Now().UTC()
			for _, row := range rows {
				if !strings.Contains(row.BodyMD, "upload://") {
					continue
				}
				html, err := render.RenderMarkdown(row.BodyMD)
				if err != nil {
					fmt.Fprintf(os.Stderr, "issue %s: render: %v\n", row.ID, err)
					continue
				}
				if err := projsRepo.UpdateIssueBody(ctx, row.ID, row.BodyMD, html, now); err != nil {
					fmt.Fprintf(os.Stderr, "issue %s: %v\n", row.ID, err)
					continue
				}
				fixed++
			}
			fmt.Printf("re-rendered %d issue bodies containing upload:// markers\n", fixed)
		case "decode-bodies":
			ingestRows, err := mRepo.AllIngestBodies(ctx)
			if err != nil {
				return err
			}
			fixedIngests := 0
			for _, row := range ingestRows {
				decoded, ok := mailbox.TryDecodeBase64Text(row.BodyText)
				if !ok {
					continue
				}
				if err := mRepo.UpdateIngestBody(ctx, row.ID, decoded); err != nil {
					fmt.Fprintf(os.Stderr, "ingest %s: %v\n", row.ID, err)
					continue
				}
				fixedIngests++
			}
			projsRepo := projects.NewRepo(db)
			issueRows, err := projsRepo.AllIssueBodies(ctx)
			if err != nil {
				return err
			}
			fixedIssues := 0
			now := time.Now().UTC()
			for _, row := range issueRows {
				decoded, ok := mailbox.TryDecodeBase64Text(row.BodyMD)
				if !ok {
					continue
				}
				html, err := render.RenderMarkdown(decoded)
				if err != nil {
					fmt.Fprintf(os.Stderr, "issue %s: render: %v\n", row.ID, err)
					continue
				}
				if err := projsRepo.UpdateIssueBody(ctx, row.ID, decoded, html, now); err != nil {
					fmt.Fprintf(os.Stderr, "issue %s: %v\n", row.ID, err)
					continue
				}
				fixedIssues++
			}
			fmt.Printf("decoded %d ingest bodies + %d issue bodies\n", fixedIngests, fixedIssues)
		case "prune-skipped":
			names, n, err := mRepo.PruneSkippedFolderIngest(ctx, acc.ID)
			if err != nil {
				return err
			}
			if len(names) == 0 {
				fmt.Println("nothing to prune — no Sent/Drafts/Trash/Spam/All-Mail folders found for this account")
				break
			}
			fmt.Printf("pruned %d ingest rows from %d folders: %s\n", n, len(names), strings.Join(names, ", "))
		case "apply-filter":
			if len(os.Args) < 4 {
				return errors.New("usage: mailbox apply-filter <filter-id>")
			}
			filterID := os.Args[3]
			projsRepo := projects.NewRepo(db)
			projsSvc := projects.NewService(projsRepo, projects.NewBus(), nil, cfg.EditGrace)
			mboxSvc := mailbox.NewService(mRepo, mailbox.AccountConfig{}, projsSvc, projsRepo, aRepo, cfg.MailboxSystemUserID)
			matched, issued, err := mboxSvc.ApplyFilterToPast(ctx, filterID)
			if err != nil {
				return err
			}
			fmt.Printf("apply-filter %s: tagged %d past ingest rows, created %d issues\n", filterID, matched, issued)
		case "reprocess-filter":
			if len(os.Args) < 4 {
				return errors.New("usage: mailbox reprocess-filter <filter-id>")
			}
			filterID := os.Args[3]
			filter, err := mRepo.FilterByID(ctx, filterID)
			if err != nil {
				return fmt.Errorf("filter lookup: %w", err)
			}
			if !filter.ToIssue {
				return fmt.Errorf("filter %s has to_issue=false — reprocess only makes sense for issue-creating filters", filterID)
			}
			pendings, err := mRepo.IngestsByFilter(ctx, filterID)
			if err != nil {
				return err
			}
			if len(pendings) == 0 {
				fmt.Println("nothing to reprocess — every matched row already has an issue")
				break
			}
			projsRepo := projects.NewRepo(db)
			projsSvc := projects.NewService(projsRepo, projects.NewBus(), nil, cfg.EditGrace)
			mboxSvc := mailbox.NewService(mRepo, mailbox.AccountConfig{}, projsSvc, projsRepo, aRepo, cfg.MailboxSystemUserID)
			ok, fail := 0, 0
			for _, p := range pendings {
				if _, err := mboxSvc.AutoCreateIssue(ctx, mailbox.AutoCreateIssueInput{
					IngestID:    p.ID,
					CommunityID: p.CommunityID,
					Subject:     p.Subject,
					TextBody:    p.BodyText,
					HTMLBody:    "",
				}); err != nil {
					fmt.Fprintf(os.Stderr, "ingest %s: %v\n", p.ID, err)
					fail++
					continue
				}
				ok++
			}
			fmt.Printf("reprocessed %d / %d ingests for filter %s (failures: %d)\n", ok, len(pendings), filterID, fail)
		default:
			return fmt.Errorf("unknown mailbox subcommand: %s", os.Args[2])
		}
	case "project":
		if len(os.Args) < 3 {
			return errors.New("usage: project mv-attachment <attachment-id> <to-project-id>")
		}
		switch os.Args[2] {
		case "mv-attachment":
			if len(os.Args) < 5 {
				return errors.New("usage: project mv-attachment <attachment-id> <to-project-id>")
			}
			attID, toProjectID := os.Args[3], os.Args[4]
			pRepo := projects.NewRepo(db)
			if _, err := pRepo.ByID(ctx, toProjectID); err != nil {
				return fmt.Errorf("destination project not found: %w", err)
			}
			from, err := pRepo.AttachmentByID(ctx, attID)
			if err != nil {
				return fmt.Errorf("attachment lookup: %w", err)
			}
			if _, err := db.ExecContext(ctx, `
				UPDATE project_attachments SET project_id = ? WHERE id = ?`,
				toProjectID, attID); err != nil {
				return fmt.Errorf("update project_attachments: %w", err)
			}
			fmt.Printf("moved attachment %s (%s) from project %s → %s\n",
				attID, from.Filename, from.ProjectID, toProjectID)
		default:
			return fmt.Errorf("unknown project subcommand: %s", os.Args[2])
		}
	case "reindex":
		// Re-queue community-public content for RAG embedding. The running
		// server's worker drains the queue and (re)embeds. This does NOT drop
		// existing vectors — for a clean rebuild or a backend switch, stop the
		// server, delete RAG_STORE_PATH, run this, then start the server. (The
		// in-process /superadmin and /admin buttons DO drop first.)
		ragRepo := rag.NewRepo(db)
		if len(os.Args) >= 3 && os.Args[2] != "" && os.Args[2] != "all" {
			target, err := cRepo.BySlug(ctx, os.Args[2])
			if err != nil {
				return fmt.Errorf("community %q: %w", os.Args[2], err)
			}
			n, err := ragRepo.EnqueueCommunity(ctx, target.ID)
			if err != nil {
				return err
			}
			fmt.Printf("queued reindex for community %q — %d jobs pending\n", target.Slug, n)
			return nil
		}
		n, err := ragRepo.EnqueueAll(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("queued reindex for all communities — %d jobs pending\n", n)
	default:
		usage()
		return fmt.Errorf("unknown command: %s", os.Args[1])
	}
	return nil
}
