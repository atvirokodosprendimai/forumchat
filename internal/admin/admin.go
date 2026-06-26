package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/aiusage"
	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/config"
	"github.com/atvirokodosprendimai/forumchat/internal/provision"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	"github.com/atvirokodosprendimai/forumchat/internal/uploads"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// RosterNotifier lets admin mutations nudge every open presence sidebar
// to re-render (role badge / banned state changed in the DB). Satisfied
// by *presence.Tracker.Bump. Optional — nil-safe.
type RosterNotifier interface {
	Bump(communityID string)
}

// Reindexer triggers a RAG re-embed for one community. Implemented by
// *rag.Service; nil when RAG is disabled (the admin page hides the button).
type Reindexer interface {
	ReindexCommunity(ctx context.Context, communityID string) (int, error)
}

// BillingCheckout creates a Stripe Checkout Session for paid platform AI.
// Implemented by *billing.Service; nil (or Enabled()==false) hides the Subscribe
// button and 404s the checkout route. Declared here so admin never imports
// billing (which imports stripe).
type BillingCheckout interface {
	Enabled() bool
	Checkout(ctx context.Context, communityID, slug string) (string, error)
}

type Handler struct {
	Repo        *auth.Repo
	Svc         *auth.Service
	Chat        *chat.Handler
	Communities *community.Repo
	// Provision creates a community + its #general + first member in the correct
	// order. Shared with the super-admin and SaaS self-serve create paths.
	Provision *provision.Service
	// Roster, when set, is pinged after member-state mutations so the
	// chat roster reflects role/ban changes without waiting for the next
	// presence heartbeat.
	Roster RosterNotifier
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
	// RAG triggers a per-community vector reindex. Nil when RAG is disabled.
	RAG Reindexer
	// Cfg is the platform config, used by the owner Settings page to resolve
	// per-community tenant config against env defaults.
	Cfg config.Config
	// Uploads is used by the owner Storage card to migrate a community to its
	// own S3 bucket. Optional — nil disables the migrate action.
	Uploads *uploads.Store
	// Usage is the platform-AI metering ledger, read for the owner's own usage
	// summary on the Platform AI card. Nil-safe.
	Usage *aiusage.Recorder
	// Billing creates Stripe checkout sessions for paid platform AI. Nil (or
	// disabled) hides the Subscribe button. Nil-safe via billingEnabled().
	Billing BillingCheckout
}

// billingEnabled reports whether a Stripe Subscribe path is available.
func (h *Handler) billingEnabled() bool {
	return h.Billing != nil && h.Billing.Enabled()
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

// membershipInCommunity loads the membership by id and confirms it belongs
// to the admin's URL-slug community. These handlers are RequireRole(admin)
// for the *slug* community only, but acted on a raw query-param membership
// id — so an admin of community A could ban/remove/approve/role-change any
// member of community B by passing a foreign id. Writes 404 and returns
// ok=false on miss or cross-tenant mismatch. (Super-admin cross-community
// moderation lives in internal/superadmin, by design.)
func (h *Handler) membershipInCommunity(w http.ResponseWriter, r *http.Request, membershipID string) (auth.Membership, bool) {
	m, err := h.Repo.MembershipByID(r.Context(), membershipID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return auth.Membership{}, false
	}
	if m.CommunityID != h.cid(r) {
		http.NotFound(w, r)
		return auth.Membership{}, false
	}
	return m, true
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

// PostReindex drops this community's vector index and re-queues its public
// content for embedding. The embed worker drains the queue in the background.
func (h *Handler) PostReindex(w http.ResponseWriter, r *http.Request) {
	sse := datastar.NewSSE(w, r)
	if h.RAG == nil {
		_ = sse.PatchElementTempl(webtempl.AdminReindexResult("RAG is disabled on this instance."))
		return
	}
	n, err := h.RAG.ReindexCommunity(r.Context(), h.cid(r))
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.AdminReindexResult("Reindex failed: " + err.Error()))
		return
	}
	_ = sse.PatchElementTempl(webtempl.AdminReindexResult(
		fmt.Sprintf("Reindex queued — %d jobs. The embed worker is processing them in the background.", n)))
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
	reports, err := h.Repo.ListOpenReports(r.Context(), h.cid(r))
	if err != nil {
		http.Error(w, "load reports: "+err.Error(), http.StatusInternalServerError)
		return
	}

	now := time.Now()
	isPublic := false
	ratePerUser, ratePerCommunity := 0, 0
	if c, ok := community.FromContext(r.Context()); ok {
		isPublic = c.IsPublic
		ratePerUser = c.AgentRatePerUserMin
		ratePerCommunity = c.AgentRatePerCommunityMin
	}
	data := webtempl.AdminPageData{
		Viewer:                   h.viewer(r),
		IsPublic:                 isPublic,
		Pending:                  memberRowsToAdminMembers(pending, now),
		Members:                  memberRowsToAdminMembers(members, now),
		Invites:                  invitesToAdminInvites(invites),
		Reports:                  reportsToAdminReports(reports),
		AgentRatePerUserMin:      ratePerUser,
		AgentRatePerCommunityMin: ratePerCommunity,
	}
	_ = webtempl.AdminPage(data).Render(r.Context(), w)
}

func (h *Handler) PostApprove(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	m, ok := h.membershipInCommunity(w, r, id)
	if !ok {
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
		h.Chat.Welcome(r.Context(), m.CommunityID, m.ShownName())
	}
	h.bumpRoster(r)
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

// PostSetAgentLimits saves the community's AI-agent prompt rate limits
// (requests/minute, 0 = unlimited). The shared Gate reads these fresh on every
// check, so changes apply immediately — no restart. Negative inputs clamp to 0.
func (h *Handler) PostSetAgentLimits(w http.ResponseWriter, r *http.Request) {
	c, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	// ReadSignals MUST come before NewSSE — NewSSE closes the request body.
	var in struct {
		PerUser      int `json:"agent_rate_user"`
		PerCommunity int `json:"agent_rate_community"`
	}
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals", http.StatusBadRequest)
		return
	}
	if err := h.Communities.SetAgentRateLimits(r.Context(), c.ID, in.PerUser, in.PerCommunity); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if in.PerUser < 0 {
		in.PerUser = 0
	}
	if in.PerCommunity < 0 {
		in.PerCommunity = 0
	}
	sse := render.NewSSE(w, r)
	_ = sse.PatchElementTempl(webtempl.AdminAgentLimitsSaved(in.PerUser, in.PerCommunity))
}

func (h *Handler) PostReject(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if _, ok := h.membershipInCommunity(w, r, id); !ok {
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
	m, ok := h.membershipInCommunity(w, r, id)
	if !ok {
		return
	}
	// Same guards PostRemoveMember already has: an admin can't lock
	// themselves out, and can't ban a fellow admin/owner through this path
	// (which would orphan the community's privileged access).
	if vid, ok := auth.FromContext(r.Context()); ok && vid.User.ID == m.UserID {
		http.Error(w, "cannot ban yourself", http.StatusBadRequest)
		return
	}
	if m.Role.AtLeast(auth.RoleAdmin) {
		http.Error(w, "cannot ban an admin or owner here", http.StatusBadRequest)
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
		h.Chat.Bus.Broadcast("")
	}
	h.bumpRoster(r)
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
	m, ok := h.membershipInCommunity(w, r, membershipID)
	if !ok {
		return
	}
	if id, ok := auth.FromContext(r.Context()); ok && id.User.ID == m.UserID {
		http.Error(w, "cannot remove yourself", http.StatusBadRequest)
		return
	}
	if m.Role.AtLeast(auth.RoleAdmin) {
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
		h.Chat.Bus.Broadcast("")
	}
	h.bumpRoster(r)
	h.refreshAdminLists(w, r)
}

func (h *Handler) PostUnban(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if _, ok := h.membershipInCommunity(w, r, id); !ok {
		return
	}
	if err := h.Repo.UpdateBan(r.Context(), id, nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.bumpRoster(r)
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
	if reports, err := h.Repo.ListOpenReports(r.Context(), h.cid(r)); err == nil {
		_ = sse.PatchElementTempl(webtempl.AdminReports(h.cslug(r), reportsToAdminReports(reports)))
	}
}

// PostResolveReport marks a moderation report resolved, dropping it from
// the open queue. Admin-gated by the route.
func (h *Handler) PostResolveReport(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if err := h.Repo.ResolveUserReport(r.Context(), id, h.cid(r)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.refreshAdminLists(w, r)
}

// bumpRoster re-renders every open chat roster so role/ban changes show
// without waiting for a presence heartbeat. No-op when Roster is unset.
func (h *Handler) bumpRoster(r *http.Request) {
	if h.Roster != nil {
		h.Roster.Bump(h.cid(r))
	}
}

// PostSetRole promotes a member to moderator or demotes a moderator back
// to member. Reached from the chat roster's right-click menu (admin-only
// route). role + target id arrive as query params. Guards: can't change
// your own role, can't touch an admin's role through this path (admins
// are managed via the CLI / explicit flows).
func (h *Handler) PostSetRole(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	role := auth.Role(r.URL.Query().Get("role"))
	if id == "" || (role != auth.RoleMember && role != auth.RoleMod) {
		http.Error(w, "missing id or invalid role", http.StatusBadRequest)
		return
	}
	m, ok := h.membershipInCommunity(w, r, id)
	if !ok {
		return
	}
	if vid, ok := auth.FromContext(r.Context()); ok && vid.User.ID == m.UserID {
		http.Error(w, "cannot change your own role", http.StatusBadRequest)
		return
	}
	if m.Role.AtLeast(auth.RoleAdmin) {
		http.Error(w, "cannot change an admin's or owner's role here", http.StatusBadRequest)
		return
	}
	if err := h.Repo.UpdateMembershipRole(r.Context(), id, role); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.bumpRoster(r)
	h.refreshAdminLists(w, r)
}

type nickSignals struct {
	AdminNick string `json:"admin_nick"`
}

// PostSetNick sets (or clears, when blank) the admin display-name override
// for a member. The override is what everyone else sees in chat, the forum,
// the roster and @mentions; the member's own self-chosen name is left intact
// as the fallback. Admin-gated by the route; cross-tenant-guarded by
// membershipInCommunity. Reached from the member row in /admin.
func (h *Handler) PostSetNick(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	// ReadSignals MUST come before NewSSE — NewSSE closes the request body.
	var in nickSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	if _, ok := h.membershipInCommunity(w, r, id); !ok {
		return
	}
	nick := strings.TrimSpace(in.AdminNick)
	if rs := []rune(nick); len(rs) > 60 {
		nick = strings.TrimSpace(string(rs[:60]))
	}
	if err := h.Repo.SetAdminDisplayName(r.Context(), id, nick); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The override changes how this member is shown to everyone, so refresh
	// open rosters and re-render the admin member list.
	h.bumpRoster(r)
	h.refreshAdminLists(w, r)
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
	display := user.Email
	if i := strings.IndexByte(display, '@'); i > 0 {
		display = display[:i]
	}
	// The first member of a brand-new community is its owner (community
	// super-admin), so in SaaS they can reach /settings and configure the tenant.
	// Harmless in self-host (owner ≥ admin; /settings unmounted). Provisioning
	// (create + seed #general + seed member) lives in one shared place.
	c, err := h.Provision.Create(r.Context(), provision.Input{
		Slug: slug, Name: name, OwnerUserID: user.ID,
		DisplayName: display, Role: auth.RoleOwner,
	})
	if err != nil {
		if errors.Is(err, community.ErrSlugTaken) {
			_ = sse.PatchElementTempl(webtempl.ErrorFragment("cc-error", "Slug already in use"))
			return
		}
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("cc-error", err.Error()))
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
			DisplayName:  r.EffectiveDisplayName,
			RealName:     r.Membership.DisplayName,
			AdminNick:    r.AdminDisplayName,
			Role:         string(r.Role),
			IsBanned:     r.IsBanned(now),
			BannedUntil:  r.BannedUntil,
			IsApproved:   r.IsApproved(),
			CreatedAt:    r.CreatedAt,
			JoinReason:   r.JoinReason,
		}
		out = append(out, am)
	}
	return out
}

func reportsToAdminReports(rows []auth.UserReport) []webtempl.AdminReport {
	out := make([]webtempl.AdminReport, 0, len(rows))
	for _, rep := range rows {
		out = append(out, webtempl.AdminReport{
			ID:           rep.ID,
			ReporterName: rep.ReporterName,
			ReportedName: rep.ReportedName,
			Reason:       rep.Reason,
			When:         rep.CreatedAt.Local().Format("15:04 Jan 2"),
			// context_ref is "chat:<msgID>" when the report came from a
			// message's ⋮ menu; flag it so the queue shows the ⚑ chip.
			OnMessage: strings.HasPrefix(rep.ContextRef, "chat:"),
		})
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
