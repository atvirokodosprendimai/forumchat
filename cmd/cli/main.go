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
  forumchat-cli invite [count]                  create N invite codes for bootstrap community (default 1)
  forumchat-cli role <email> <member|moderator|admin>
  forumchat-cli ban <email> [duration]          ban member; duration like 24h, omit for permanent
  forumchat-cli unban <email>
`)
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
		if len(os.Args) >= 3 {
			fmt.Sscanf(os.Args[2], "%d", &n)
			if n < 1 {
				n = 1
			}
		}
		for i := 0; i < n; i++ {
			code, err := svc.IssueInvite(ctx, c.ID, nil)
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
			return errors.New("usage: ban <email> [duration]")
		}
		email := os.Args[2]
		var until *time.Time
		if len(os.Args) >= 4 {
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
		fmt.Printf("banned %s until %s\n", email, until.Format(time.RFC3339))
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
