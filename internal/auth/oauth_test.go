package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
)

func oauthInput(provider, providerUserID, email, name string) auth.OAuthInput {
	return auth.OAuthInput{
		Provider:       provider,
		ProviderUserID: providerUserID,
		Email:          email,
		Name:           name,
		AvatarURL:      "https://example.test/a.png",
	}
}

func TestUpsertOAuthUser_NewEmailRequiresOpenRegistration(t *testing.T) {
	svc, _, _ := setupSvc(t)
	svc.OpenRegistration = false

	_, err := svc.UpsertOAuthUser(context.Background(), oauthInput("google", "g-1", "new@example.com", "New User"))
	if !errors.Is(err, auth.ErrOAuthNoAccount) {
		t.Fatalf("want ErrOAuthNoAccount, got %v", err)
	}
}

func TestUpsertOAuthUser_NewEmailCreatesAndLinks(t *testing.T) {
	svc, repo, cid := setupSvc(t)
	svc.OpenRegistration = true
	ctx := context.Background()

	res, err := svc.UpsertOAuthUser(ctx, oauthInput("google", "g-42", "Fresh@Example.com", "Fresh Person"))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if res.User.Status != auth.StatusActive {
		t.Fatalf("want active user, got %q", res.User.Status)
	}
	if res.User.Email != "fresh@example.com" {
		t.Fatalf("email not normalised: %q", res.User.Email)
	}
	if res.Membership.CommunityID != cid {
		t.Fatalf("membership community = %q, want %q", res.Membership.CommunityID, cid)
	}
	if res.Membership.DisplayName != "Fresh Person" {
		t.Fatalf("display name = %q, want provider name", res.Membership.DisplayName)
	}

	// Identity is now linked: a second sign-in resolves to the SAME user even
	// with open registration turned back off, and creates no duplicate.
	svc.OpenRegistration = false
	res2, err := svc.UpsertOAuthUser(ctx, oauthInput("google", "g-42", "fresh@example.com", "Fresh Person"))
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if res2.User.ID != res.User.ID {
		t.Fatalf("second sign-in made a new user: %q vs %q", res2.User.ID, res.User.ID)
	}
	if uid, err := repo.UserIDByIdentity(ctx, "google", "g-42"); err != nil || uid != res.User.ID {
		t.Fatalf("identity lookup = (%q, %v), want %q", uid, err, res.User.ID)
	}
}

func TestUpsertOAuthUser_LinksToExistingPasswordAccount(t *testing.T) {
	svc, repo, cid := setupSvc(t)
	svc.OpenRegistration = false // closed reg must NOT block linking an existing user
	ctx := context.Background()

	// Seed an existing active password account + membership.
	u := auth.User{ID: uuid.NewString(), Email: "existing@example.com", PasswordHash: "$2a$bogus", Status: auth.StatusActive}
	if err := repo.CreateUser(ctx, u); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := repo.CreateMembership(ctx, nil, auth.Membership{
		ID: uuid.NewString(), UserID: u.ID, CommunityID: cid, DisplayName: "Existing", Role: auth.RoleMember,
	}); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	res, err := svc.UpsertOAuthUser(ctx, oauthInput("facebook", "fb-9", "EXISTING@example.com", "Existing"))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if res.User.ID != u.ID {
		t.Fatalf("linked to wrong user: %q want %q", res.User.ID, u.ID)
	}
	if uid, err := repo.UserIDByIdentity(ctx, "facebook", "fb-9"); err != nil || uid != u.ID {
		t.Fatalf("identity not linked: (%q, %v)", uid, err)
	}
}

func TestUpsertOAuthUser_NoEmailRejected(t *testing.T) {
	svc, _, _ := setupSvc(t)
	svc.OpenRegistration = true

	_, err := svc.UpsertOAuthUser(context.Background(), oauthInput("facebook", "fb-x", "", "No Email"))
	if !errors.Is(err, auth.ErrOAuthNoEmail) {
		t.Fatalf("want ErrOAuthNoEmail, got %v", err)
	}
}

func TestUpsertOAuthUser_DisabledAccountRefused(t *testing.T) {
	svc, repo, _ := setupSvc(t)
	svc.OpenRegistration = true
	ctx := context.Background()

	u := auth.User{ID: uuid.NewString(), Email: "disabled@example.com", PasswordHash: "$2a$bogus", Status: auth.StatusDisabled}
	if err := repo.CreateUser(ctx, u); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	_, err := svc.UpsertOAuthUser(ctx, oauthInput("google", "g-dis", "disabled@example.com", "Disabled"))
	if !errors.Is(err, auth.ErrBanned) {
		t.Fatalf("want ErrBanned, got %v", err)
	}
}
