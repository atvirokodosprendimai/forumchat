package admin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

type Handler struct {
	Repo        *auth.Repo
	Svc         *auth.Service
	Chat        *chat.Handler
	Communities *community.Repo
	// Mail is optional. When set, "Add member by email" can email the
	// join link directly to the recipient (existing or invited user).
	Mail auth.Mailer
	// BaseURL is the configured public URL of the instance. When the
	// incoming request lacks usable scheme/host headers (e.g. background
	// flows), we fall back to this for absolute URLs in emails.
	BaseURL       string
	CommunityID   string
	CommunityName string
	Log           *slog.Logger
}

func (h *Handler) cid(r *http.Request) string {
	if c, ok := community.FromContext(r.Context()); ok {
		return c.ID
	}
	return h.CommunityID
}

func (h *Handler) cname(r *http.Request) string {
	if c, ok := community.FromContext(r.Context()); ok {
		return c.Name
	}
	return h.CommunityName
}

func (h *Handler) cslug(r *http.Request) string {
	if c, ok := community.FromContext(r.Context()); ok {
		return c.Slug
	}
	return ""
}

func (h *Handler) viewer(r *http.Request) webtempl.Viewer {
	v := webtempl.Viewer{CommunityName: h.cname(r), CommunitySlug: h.cslug(r)}
	if id, ok := auth.FromContext(r.Context()); ok {
		v.IsAuthed = true
		v.DisplayName = id.Membership.DisplayName
		v.Role = string(id.Membership.Role)
	}
	return v
}

// GetIndex renders the admin dashboard with pending requests, members, invites.
func (h *Handler) GetIndex(w http.ResponseWriter, r *http.Request) {
	pending, err := h.Repo.ListPendingMemberships(r.Context(), h.cid(r))
	if err != nil {
		http.Error(w, "load pending: "+err.Error(), http.StatusInternalServerError)
		return
	}
	members, err := h.Repo.ListMembers(r.Context(), h.cid(r))
	if err != nil {
		http.Error(w, "load members: "+err.Error(), http.StatusInternalServerError)
		return
	}
	invites, err := h.Repo.ListInvites(r.Context(), h.cid(r))
	if err != nil {
		http.Error(w, "load invites: "+err.Error(), http.StatusInternalServerError)
		return
	}

	now := time.Now()
	isPublic := false
	if c, ok := community.FromContext(r.Context()); ok {
		isPublic = c.IsPublic
	}
	data := webtempl.AdminPageData{
		Viewer:   h.viewer(r),
		IsPublic: isPublic,
		Pending:  memberRowsToAdminMembers(pending, now),
		Members:  memberRowsToAdminMembers(members, now),
		Invites:  invitesToAdminInvites(invites),
	}
	_ = webtempl.AdminPage(data).Render(r.Context(), w)
}

func (h *Handler) PostApprove(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if err := h.Repo.ApproveMembership(r.Context(), id); err != nil && !errors.Is(err, auth.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Drop a "say hello" notice into the chat so the rest of the community
	// notices the new member. Best-effort — if any of this fails the approve
	// itself still succeeded.
	if h.Chat != nil {
		if m, err := h.Repo.MembershipByID(r.Context(), id); err == nil {
			h.Chat.Welcome(r.Context(), m.CommunityID, m.DisplayName)
		}
	}
	h.refreshAdminLists(w, r)
}

// PostTogglePublic flips the community's discoverability flag. Visible
// communities show up on /explore so signed-in users can request to join.
func (h *Handler) PostTogglePublic(w http.ResponseWriter, r *http.Request) {
	c, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := h.Communities.SetPublic(r.Context(), c.ID, !c.IsPublic); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + c.Slug + "/admin")
}

func (h *Handler) PostReject(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if err := h.Repo.RejectMembership(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.refreshAdminLists(w, r)
}

type banSignals struct {
	BanHours       int  `json:"ban_hours"`
	CleanupChat    bool `json:"cleanup_chat"`
	CleanupThreads bool `json:"cleanup_threads"`
	CleanupPosts   bool `json:"cleanup_posts"`
}

func (h *Handler) PostBan(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	var in banSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	m, err := h.Repo.MembershipByID(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	var until time.Time
	if in.BanHours <= 0 {
		until = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	} else {
		until = time.Now().Add(time.Duration(in.BanHours) * time.Hour)
	}
	if err := h.Repo.UpdateBan(r.Context(), id, &until); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	opts := auth.CleanupOptions{Chat: in.CleanupChat, Threads: in.CleanupThreads, Posts: in.CleanupPosts}
	if opts.Chat || opts.Threads || opts.Posts {
		if err := h.Repo.CleanupUserContent(r.Context(), m.UserID, h.cid(r), opts); err != nil {
			h.Log.Error("cleanup on ban", "err", err)
		}
	}
	// If any chat content was wiped, push a chat fan-out so open chat tabs refresh.
	if opts.Chat && h.Chat != nil {
		h.Chat.Bus.Broadcast()
	}
	h.refreshAdminLists(w, r)
}

type removeSignals struct {
	CleanupChat    bool `json:"cleanup_chat"`
	CleanupThreads bool `json:"cleanup_threads"`
	CleanupPosts   bool `json:"cleanup_posts"`
}

// PostRemoveMember hard-deletes the membership row, optionally
// soft-deleting the user's chat/forum content first. Guarded so the
// admin can't pull the rug out from under themselves (self-removal) or
// orphan the community (removing the last admin). The user account
// itself stays — they can rejoin later with a fresh invite.
func (h *Handler) PostRemoveMember(w http.ResponseWriter, r *http.Request) {
	membershipID := r.URL.Query().Get("id")
	if membershipID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	var in removeSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	m, err := h.Repo.MembershipByID(r.Context(), membershipID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if id, ok := auth.FromContext(r.Context()); ok && id.User.ID == m.UserID {
		http.Error(w, "cannot remove yourself", http.StatusBadRequest)
		return
	}
	if m.Role == auth.RoleAdmin {
		count, err := h.Repo.CountAdmins(r.Context(), h.cid(r))
		if err != nil {
			http.Error(w, "count admins: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if count <= 1 {
			http.Error(w, "cannot remove the last admin", http.StatusBadRequest)
			return
		}
	}
	opts := auth.CleanupOptions{Chat: in.CleanupChat, Threads: in.CleanupThreads, Posts: in.CleanupPosts}
	if opts.Chat || opts.Threads || opts.Posts {
		if err := h.Repo.CleanupUserContent(r.Context(), m.UserID, h.cid(r), opts); err != nil {
			h.Log.Error("cleanup on remove", "err", err)
		}
	}
	if err := h.Repo.RejectMembership(r.Context(), membershipID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if opts.Chat && h.Chat != nil {
		h.Chat.Bus.Broadcast()
	}
	h.refreshAdminLists(w, r)
}

func (h *Handler) PostUnban(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if err := h.Repo.UpdateBan(r.Context(), id, nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.refreshAdminLists(w, r)
}

type inviteSignals struct {
	MaxUses string `json:"max_uses"`
}

func (h *Handler) PostInvite(w http.ResponseWriter, r *http.Request) {
	var in inviteSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)
	var maxUses *int
	if v := strings.TrimSpace(in.MaxUses); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			_ = sse.PatchElementTempl(webtempl.ErrorFragment("admin-invite-error", "Max uses must be a positive integer or blank for unlimited"))
			return
		}
		maxUses = &n
	}
	id, _ := auth.FromContext(r.Context())
	creator := id.User.ID
	code, err := h.Svc.IssueInvite(r.Context(), h.cid(r), &creator, maxUses)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("admin-invite-error", err.Error()))
		return
	}
	_ = sse.PatchElementTempl(webtempl.AdminInviteCreated(code))
	// Re-render the invite list.
	if list, err := h.Repo.ListInvites(r.Context(), h.cid(r)); err == nil {
		_ = sse.PatchElementTempl(webtempl.AdminInvites(h.cslug(r), invitesToAdminInvites(list)))
	}
}

func (h *Handler) PostInviteRevoke(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}
	if err := h.Repo.RevokeInvite(r.Context(), code); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sse := render.NewSSE(w, r)
	if list, err := h.Repo.ListInvites(r.Context(), h.cid(r)); err == nil {
		_ = sse.PatchElementTempl(webtempl.AdminInvites(h.cslug(r), invitesToAdminInvites(list)))
	}
}

// refreshAdminLists re-renders #admin-pending and #admin-members after an
// admin action that changed the queue or member list.
func (h *Handler) refreshAdminLists(w http.ResponseWriter, r *http.Request) {
	sse := render.NewSSE(w, r)
	now := time.Now()
	if pending, err := h.Repo.ListPendingMemberships(r.Context(), h.cid(r)); err == nil {
		_ = sse.PatchElementTempl(webtempl.AdminPending(h.cslug(r), memberRowsToAdminMembers(pending, now)))
	}
	if members, err := h.Repo.ListMembers(r.Context(), h.cid(r)); err == nil {
		_ = sse.PatchElementTempl(webtempl.AdminMembers(h.cslug(r), memberRowsToAdminMembers(members, now)))
	}
}

type createCommunitySignals struct {
	Name        string `json:"cc_name"`
	Slug        string `json:"cc_slug"`
	MemberEmail string `json:"cc_member_email"`
}

// PostCreateCommunity is the global-admin-only handler for spinning up a new
// community. The slug must be unique; the named user becomes the community's
// first member with role=admin.
func (h *Handler) PostCreateCommunity(w http.ResponseWriter, r *http.Request) {
	// ReadSignals MUST come before NewSSE — NewSSE closes the request body.
	var in createCommunitySignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("cc-error", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)
	name := strings.TrimSpace(in.Name)
	slug := strings.ToLower(strings.TrimSpace(in.Slug))
	email := strings.TrimSpace(in.MemberEmail)
	if name == "" || slug == "" || email == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("cc-error", "Name, slug and first-member email are required"))
		return
	}
	if !validSlug(slug) {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("cc-error", "Slug must contain only a-z, 0-9, '-'"))
		return
	}
	user, err := h.Repo.UserByEmail(r.Context(), email)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("cc-error", "No user with that email"))
		return
	}
	c, err := h.Communities.Create(r.Context(), slug, name)
	if err != nil {
		if errors.Is(err, community.ErrSlugTaken) {
			_ = sse.PatchElementTempl(webtempl.ErrorFragment("cc-error", "Slug already in use"))
			return
		}
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("cc-error", err.Error()))
		return
	}
	now := time.Now()
	display := user.Email
	if i := strings.IndexByte(display, '@'); i > 0 {
		display = display[:i]
	}
	m := auth.Membership{
		ID:          uuid.NewString(),
		UserID:      user.ID,
		CommunityID: c.ID,
		DisplayName: display,
		Role:        auth.RoleAdmin,
		ApprovedAt:  &now,
	}
	if err := h.Repo.CreateMembership(r.Context(), nil, m); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("cc-error", "Could not add first member: "+err.Error()))
		return
	}
	_ = sse.Redirect("/c/" + c.Slug + "/chat")
}

var slugRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func validSlug(s string) bool { return slugRE.MatchString(s) }

type addMemberSignals struct {
	Email     string `json:"am_email"`
	Role      string `json:"am_role"`
	SendEmail bool   `json:"am_send_email"`
}

// PostAddMember is the admin "click click edit and done" add-by-email handler.
//   - existing user: insert pre-approved membership.
//   - new email: create placeholder user (status=invited) + pre-approved
//     membership + signup token; render the copy-able join URL.
func (h *Handler) PostAddMember(w http.ResponseWriter, r *http.Request) {
	if h.Communities == nil {
		http.Error(w, "communities repo not wired", http.StatusInternalServerError)
		return
	}
	var in addMemberSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("am-result", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)

	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" || !strings.Contains(email, "@") {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("am-result", "Valid email required"))
		return
	}
	role := auth.RoleMember
	if strings.EqualFold(in.Role, "moderator") {
		role = auth.RoleMod
	}

	cid := h.cid(r)
	cslug := h.cslug(r)

	cname := h.cname(r)

	user, err := h.Repo.UserByEmail(r.Context(), email)
	now := time.Now()
	if err == nil {
		// existing user — check for duplicate membership
		if _, mErr := h.Repo.MembershipFor(r.Context(), user.ID, cid); mErr == nil {
			_ = sse.PatchElementTempl(webtempl.ErrorFragment("am-result", "Already a member"))
			return
		}
		display := user.Email
		if i := strings.IndexByte(display, '@'); i > 0 {
			display = display[:i]
		}
		if err := h.Repo.CreateMembership(r.Context(), nil, auth.Membership{
			ID:          uuid.NewString(),
			UserID:      user.ID,
			CommunityID: cid,
			DisplayName: display,
			Role:        role,
			ApprovedAt:  &now,
		}); err != nil {
			_ = sse.PatchElementTempl(webtempl.ErrorFragment("am-result", err.Error()))
			return
		}
		msg := "Added " + email + " — they will see this community on next sign-in."
		if in.SendEmail {
			deepLink := h.absoluteURL(r, "/c/"+cslug+"/chat")
			emailErr := h.sendCommunityWelcomeEmail(r.Context(), email, cname, deepLink)
			if emailErr != nil {
				msg = "Added " + email + ", but the email failed: " + emailErr.Error()
			} else {
				msg = "Added " + email + " and emailed them the link."
			}
		}
		_ = sse.PatchSignals([]byte(`{"am_email":""}`))
		_ = sse.PatchElementTempl(webtempl.SuccessFragment("am-result", msg))
		return
	}
	if !errors.Is(err, auth.ErrNotFound) {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("am-result", err.Error()))
		return
	}

	// new email — placeholder user + membership + signup token
	newUser, err := h.Repo.CreateInvitedUser(r.Context(), email)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("am-result", err.Error()))
		return
	}
	display := newUser.Email
	if i := strings.IndexByte(display, '@'); i > 0 {
		display = display[:i]
	}
	if err := h.Repo.CreateMembership(r.Context(), nil, auth.Membership{
		ID:          uuid.NewString(),
		UserID:      newUser.ID,
		CommunityID: cid,
		DisplayName: display,
		Role:        role,
		ApprovedAt:  &now,
	}); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("am-result", err.Error()))
		return
	}
	token, err := h.Repo.MintSignupToken(r.Context(), newUser.ID, cid, 7*24*time.Hour)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("am-result", err.Error()))
		return
	}
	joinPath := "/c/" + cslug + "/join?code=" + token
	_ = sse.PatchSignals([]byte(`{"am_email":""}`))

	if in.SendEmail {
		joinURL := h.absoluteURL(r, joinPath)
		if err := h.sendCommunityJoinEmail(r.Context(), email, cname, joinURL); err != nil {
			_ = sse.PatchElementTempl(webtempl.ErrorFragment("am-result",
				"Invite created, but the email failed: "+err.Error()+" — copy this link instead: "+joinPath))
			return
		}
		_ = sse.PatchElementTempl(webtempl.SuccessFragment("am-result",
			"Invited "+email+" — join link emailed."))
		return
	}
	_ = sse.PatchElementTempl(webtempl.InviteURLFragment("am-result", email, joinPath))
}

// absoluteURL turns a path into an absolute URL using the incoming
// request's scheme/host (honouring X-Forwarded-* headers) so the link
// works behind a reverse proxy. Falls back to the configured BaseURL
// when the request lacks host info.
func (h *Handler) absoluteURL(r *http.Request, path string) string {
	scheme := "http"
	if r != nil && r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	host := r.Host
	if hdr := r.Header.Get("X-Forwarded-Host"); hdr != "" {
		host = hdr
	}
	if host == "" {
		if h.BaseURL != "" {
			return strings.TrimRight(h.BaseURL, "/") + path
		}
		return path
	}
	return scheme + "://" + host + path
}

func (h *Handler) sendCommunityJoinEmail(ctx context.Context, to, communityName, joinURL string) error {
	if h.Mail == nil {
		return errors.New("email not configured on this instance")
	}
	subject := communityName + " — your join link"
	body := "You've been invited to join " + communityName + " on forumchat.\n\n" +
		"Click to set up your account and join:\n" + joinURL + "\n\n" +
		"This link is good for 7 days.\n"
	return h.Mail.Send(ctx, to, subject, body)
}

func (h *Handler) sendCommunityWelcomeEmail(ctx context.Context, to, communityName, deepLink string) error {
	if h.Mail == nil {
		return errors.New("email not configured on this instance")
	}
	subject := "You've been added to " + communityName
	body := "You've been added to the community " + communityName + " on forumchat.\n\n" +
		"Sign in and jump straight in:\n" + deepLink + "\n"
	return h.Mail.Send(ctx, to, subject, body)
}

func memberRowsToAdminMembers(rows []auth.MemberRow, now time.Time) []webtempl.AdminMember {
	out := make([]webtempl.AdminMember, 0, len(rows))
	for _, r := range rows {
		am := webtempl.AdminMember{
			MembershipID: r.ID,
			UserID:       r.UserID,
			Email:        r.Email,
			DisplayName:  r.DisplayName,
			Role:         string(r.Role),
			IsBanned:     r.IsBanned(now),
			BannedUntil:  r.BannedUntil,
			IsApproved:   r.IsApproved(),
			CreatedAt:    r.CreatedAt,
		}
		out = append(out, am)
	}
	return out
}

func invitesToAdminInvites(rows []auth.InviteCode) []webtempl.AdminInvite {
	out := make([]webtempl.AdminInvite, 0, len(rows))
	for _, ic := range rows {
		maxStr := "unlimited"
		if ic.MaxUses != nil {
			maxStr = strconv.Itoa(*ic.MaxUses)
		}
		out = append(out, webtempl.AdminInvite{
			Code:       ic.Code,
			MaxUsesStr: maxStr,
			UsesCount:  ic.UsesCount,
			ExpiresAt:  ic.ExpiresAt,
			CreatedAt:  ic.CreatedAt,
		})
	}
	return out
}
