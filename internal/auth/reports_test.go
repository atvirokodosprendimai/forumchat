package auth_test

import (
	"context"
	"testing"
)

// TestUserReports_RoundTrip covers the moderation-queue persistence
// behind the roster menu's "Report to moderators" action.
func TestUserReports_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, repo, communityID := setupSvc(t)

	reporter := registerVerified(t, svc, communityID, "gil@example.com")
	reported := registerVerified(t, svc, communityID, "hal@example.com")

	if err := repo.CreateUserReport(ctx, "rep-1", reporter, reported, communityID, "spamming the channel", ""); err != nil {
		t.Fatalf("create report: %v", err)
	}

	open, err := repo.ListOpenReports(ctx, communityID)
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("want 1 open report, got %d", len(open))
	}
	got := open[0]
	if got.Status != "open" || got.Reason != "spamming the channel" || got.ReportedUserID != reported {
		t.Fatalf("unexpected report: %+v", got)
	}
	if got.ReportedName == "" || got.ReporterName == "" {
		t.Fatalf("display names not resolved: %+v", got)
	}

	if err := repo.ResolveUserReport(ctx, "rep-1"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if open, _ := repo.ListOpenReports(ctx, communityID); len(open) != 0 {
		t.Fatalf("after resolve want 0, got %d", len(open))
	}
}
