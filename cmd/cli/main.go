package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/config"
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
	default:
		usage()
		return fmt.Errorf("unknown command: %s", os.Args[1])
	}
	return nil
}
