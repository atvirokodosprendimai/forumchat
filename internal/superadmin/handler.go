// Package superadmin implements the global platform super-admin surface:
// a god-mode dashboard over every community and user, gated by
// auth.RequireSuperAdmin (the SUPERADMIN_EMAILS allowlist). Per-community
// administration still happens in each community's own /admin — a
// super-admin reaches those via the RequireMember bypass.
package superadmin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/aiusage"
	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/debuglog"
	"github.com/atvirokodosprendimai/forumchat/internal/moderation"
	"github.com/atvirokodosprendimai/forumchat/internal/provision"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// Reindexer triggers a global RAG re-embed and drops a community's vectors.
// Implemented by *rag.Service; nil when RAG is disabled (the dashboard hides the
// button), so the handler is nil-safe.
type Reindexer interface {
	ReindexAll(ctx context.Context) (int, error)
	// DropCommunity removes a community's vector collection. Called on community
	// delete so a deleted tenant's embedded content doesn't survive in Qdrant.
	DropCommunity(ctx context.Context, communityID string) error
}

// ChatBroadcaster posts a pre-rendered HTML system message into one community's
// #general and fans it out live. Implemented by *chat.Handler.SystemBroadcast;
// PostBroadcast loops it over every community for a platform announcement.
type ChatBroadcaster interface {
	SystemBroadcast(ctx context.Context, communityID, bodyHTML string) error
}

// AuthVerifier recovers signups stuck on email verification. Implemented by
// *auth.Service. ForceVerify activates a pending user with no email; resend
// re-issues the verification mail and returns the URL for manual hand-off.
type AuthVerifier interface {
	ForceVerify(ctx context.Context, userID string) error
	ResendVerification(ctx context.Context, userID string) (string, error)
}

type Handler struct {
	AuthRepo    *auth.Repo
	Communities *community.Repo
	// Provision creates a community + its #general + first member in the correct
	// order, shared with the admin/self-serve/approval create paths.
	Provision *provision.Service
	Log       *slog.Logger
	// Bus fans out a chat refresh after a system-ban wipes content so open
	// chat tabs drop the soft-deleted messages live. It is the process-wide
	// chat bus. Nil-safe (tests omit it).
	Bus *chat.Bus
	// RAG triggers a global vector reindex. Nil when RAG is disabled.
	RAG Reindexer
	// Chat fans an announcement into every community's #general live. Wired in
	// main.go to *chat.Handler. Nil-safe — the broadcast card errors cleanly.
	Chat ChatBroadcaster
	// Debug is the platform-wide debug recorder behind the in-memory on/off
	// switch the dashboard exposes. Nil-safe (its methods guard a nil receiver),
	// so the debug card simply shows "off" when unwired.
	Debug *debuglog.Recorder
	// Usage is the platform-AI metering ledger, read for the per-community cost
	// table on the platform-AI card. Nil-safe — the card shows zero usage when
	// unwired.
	Usage *aiusage.Recorder
	// Auth recovers signups stuck on email verification (force-verify / resend).
	// Wired to *auth.Service in main.go.
	Auth AuthVerifier
	// Moderation reads the raw safety-classifier audit for the moderation-log
	// card. Nil when the feature is unwired (card shows the "off" hint).
	Moderation *moderation.Repo
	// ModerationOn reflects cfg.ModerationEnabled() so the card distinguishes
	// "classifier off" from "on but nothing flagged".
	ModerationOn bool
}

// recentModerationLimit caps the raw moderation-log card.
const recentModerationLimit = 50

// usageWindow is the rolling lookback for the super-admin cost figures.
const usageWindow = 30 * 24 * time.Hour

var slugRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func validSlug(s string) bool { return slugRE.MatchString(s) }

func localPart(email string) string {
	if i := strings.IndexByte(email, '@'); i > 0 {
		return email[:i]
	}
	return email
}

func (h *Handler) viewer(r *http.Request) webtempl.Viewer {
	v := webtempl.Viewer{}
	if id, ok := auth.FromContext(r.Context()); ok {
		v.IsAuthed = true
		v.DisplayName = id.Membership.DisplayName
		v.Role = string(id.Membership.Role)
	}
	return v
}

// GetIndex renders the platform dashboard: every community and every user.
func (h *Handler) GetIndex(w http.ResponseWriter, r *http.Request) {
	comms, err := h.Communities.ListAll(r.Context())
	if err != nil {
		http.Error(w, "load communities: "+err.Error(), http.StatusInternalServerError)
		return
	}
	users, err := h.AuthRepo.ListAllUsers(r.Context())
	if err != nil {
		http.Error(w, "load users: "+err.Error(), http.StatusInternalServerError)
		return
	}
	debugCount, _ := h.Debug.Count(r.Context())
	// Pending self-serve community requests (SaaS). The query is cheap and returns
	// nothing in self-host (no requests are ever filed there); the page only shows
	// the card in SaaS via the SaaSEnabled template gate.
	reqs, err := h.Communities.ListPendingRequests(r.Context())
	if err != nil {
		http.Error(w, "load requests: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Platform-AI standings + usage (SaaS). Cheap and empty in self-host; the
	// dashboard only renders the card under the SaaSEnabled gate.
	platformAI, err := h.platformAIRows(r.Context())
	if err != nil {
		http.Error(w, "load platform AI: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Privacy-preserving abuse signals (botnet "red flags"). Computed from
	// metadata only — never message content — which is the whole point: in SaaS
	// the operator can't read a tenant's content, so this is how abuse surfaces.
	redFlags, err := h.redFlags(r.Context())
	if err != nil {
		http.Error(w, "load red flags: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Raw individual safety-classifier flags for the moderation-log card. Loaded
	// only when the repo is wired; empty otherwise (the card then shows the
	// off/empty hint based on ModerationOn).
	var modFlags []webtempl.SAModerationFlag
	if h.Moderation != nil {
		rows, err := h.Moderation.Recent(r.Context(), recentModerationLimit)
		if err != nil {
			http.Error(w, "load moderation flags: "+err.Error(), http.StatusInternalServerError)
			return
		}
		modFlags = toSAModerationFlags(rows)
	}
	data := webtempl.SuperAdminPageData{
		Viewer:          h.viewer(r),
		Communities:     toSACommunities(comms),
		Users:           toSAUsers(users),
		Requests:        toSARequests(reqs),
		PlatformAI:      platformAI,
		RedFlags:        redFlags,
		ModerationOn:    h.ModerationOn,
		ModerationFlags: modFlags,
		DebugEnabled:    h.Debug.Enabled(),
		DebugCount:      debugCount,
	}
	_ = webtempl.SuperAdminPage(data).Render(r.Context(), w)
}

func toSARequests(in []community.PendingRequest) []webtempl.SARequest {
	out := make([]webtempl.SARequest, 0, len(in))
	for _, q := range in {
		out = append(out, webtempl.SARequest{
			ID:        q.ID,
			UserEmail: q.UserEmail,
			Name:      q.Name,
			Slug:      q.Slug,
			Reason:    q.Reason,
			CreatedAt: q.CreatedAt.Format(dateFmt),
		})
	}
	return out
}

type requestDecisionSignals struct {
	ReqID string `json:"sa_req_id"`
}

// PostApproveRequest approves a pending self-serve request for an additional
// community: it provisions the community with the requester as owner, then
// stamps the request approved, and re-renders the queue.
func (h *Handler) PostApproveRequest(w http.ResponseWriter, r *http.Request) {
	var in requestDecisionSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-req-error", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)
	req, err := h.Communities.RequestByID(r.Context(), strings.TrimSpace(in.ReqID))
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-req-error", "Request not found"))
		return
	}
	if req.Status != community.RequestPending {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-req-error", "Request already decided"))
		return
	}
	user, err := h.AuthRepo.UserByID(r.Context(), req.UserID)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-req-error", "Requester's account no longer exists"))
		return
	}
	c, err := h.Provision.Create(r.Context(), provision.Input{
		Slug: req.Slug, Name: req.Name, OwnerUserID: user.ID,
		DisplayName: localPart(user.Email), Role: auth.RoleOwner,
	})
	if err != nil {
		if errors.Is(err, community.ErrSlugTaken) {
			_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-req-error", "Slug '"+req.Slug+"' is now taken — deny and ask them to pick another"))
			return
		}
		h.Log.Error("approve community request: provision", "req", req.ID, "err", err)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-req-error", "Could not create the community: "+err.Error()))
		return
	}
	if err := h.Communities.DecideRequest(r.Context(), req.ID, community.RequestApproved, deciderID(r), c.ID); err != nil {
		// The community was created; only the stamp failed. Log it — a retry would
		// hit ErrSlugTaken above, prompting the super-admin to deny the stale row.
		h.Log.Error("approve community request: stamp decision", "req", req.ID, "community", c.ID, "err", err)
	}
	h.renderRequests(r, sse)
}

// PostDenyRequest closes a pending request without creating anything.
func (h *Handler) PostDenyRequest(w http.ResponseWriter, r *http.Request) {
	var in requestDecisionSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-req-error", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)
	if err := h.Communities.DecideRequest(r.Context(), strings.TrimSpace(in.ReqID), community.RequestDenied, deciderID(r), ""); err != nil {
		if errors.Is(err, community.ErrRequestNotFound) {
			_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-req-error", "Request already decided"))
			return
		}
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-req-error", "Could not deny: "+err.Error()))
		return
	}
	h.renderRequests(r, sse)
}

// deciderID returns the acting super-admin's user id for the audit stamp.
func deciderID(r *http.Request) string {
	if id, ok := auth.FromContext(r.Context()); ok {
		return id.User.ID
	}
	return ""
}

// renderRequests reloads the pending queue and morphs the #sa-requests card.
func (h *Handler) renderRequests(r *http.Request, sse *datastar.ServerSentEventGenerator) {
	reqs, err := h.Communities.ListPendingRequests(r.Context())
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-req-error", "Could not reload requests: "+err.Error()))
		return
	}
	_ = sse.PatchElementTempl(webtempl.SARequestsCard(toSARequests(reqs)))
}

// platformAISignals carries the target community for a grant/revoke action.
type platformAISignals struct {
	CommunityID string `json:"sa_pai_cid"`
}

// PostGrantPlatformAI sponsors a community's platform AI for free (no Stripe),
// then morphs the card. Super-admin gated by the route.
func (h *Handler) PostGrantPlatformAI(w http.ResponseWriter, r *http.Request) {
	var in platformAISignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-pai-error", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)
	cid := strings.TrimSpace(in.CommunityID)
	if cid != "" {
		if err := h.Communities.GrantPlatformAI(r.Context(), cid); err != nil {
			_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-pai-error", "Could not grant: "+err.Error()))
			return
		}
		h.audit(r, "granted free platform AI", "community_id", cid)
	}
	h.renderPlatformAI(r, sse)
}

// PostRevokePlatformAI removes a free grant (a paying customer stays authorized
// via their subscription), then morphs the card.
func (h *Handler) PostRevokePlatformAI(w http.ResponseWriter, r *http.Request) {
	var in platformAISignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-pai-error", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)
	cid := strings.TrimSpace(in.CommunityID)
	if cid != "" {
		if err := h.Communities.RevokePlatformAI(r.Context(), cid); err != nil {
			_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-pai-error", "Could not revoke: "+err.Error()))
			return
		}
		h.audit(r, "revoked free platform AI", "community_id", cid)
	}
	h.renderPlatformAI(r, sse)
}

// renderPlatformAI reloads the standings + usage and morphs the #sa-platform-ai card.
func (h *Handler) renderPlatformAI(r *http.Request, sse *datastar.ServerSentEventGenerator) {
	rows, err := h.platformAIRows(r.Context())
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-pai-error", "Could not reload: "+err.Error()))
		return
	}
	_ = sse.PatchElementTempl(webtempl.SAPlatformAICard(rows))
}

// platformAIRows joins each engaged community's opt-in standing with its rolling
// platform-compute usage for the cost table.
func (h *Handler) platformAIRows(ctx context.Context) ([]webtempl.SAPlatformAIRow, error) {
	reqs, err := h.Communities.ListPlatformAIRequests(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	usage := map[string]aiusage.CommunityTotal{}
	if totals, err := h.Usage.CommunityTotals(ctx, now.Add(-usageWindow).Unix(), now.Unix()); err == nil {
		for _, t := range totals {
			usage[t.CommunityID] = t
		}
	}
	out := make([]webtempl.SAPlatformAIRow, 0, len(reqs))
	for _, q := range reqs {
		u := usage[q.CommunityID]
		requested := ""
		if q.RequestedAt != 0 {
			requested = time.Unix(q.RequestedAt, 0).Format(dateFmt)
		}
		out = append(out, webtempl.SAPlatformAIRow{
			CommunityID: q.CommunityID,
			Name:        q.Name,
			Slug:        q.Slug,
			Status:      q.Status,
			GrantedFree: q.GrantedFree,
			Subscribed:  q.Subscribed,
			On:          q.On,
			Requested:   requested,
			Requests:    u.Requests,
			TokensIn:    u.TokensIn,
			TokensOut:   u.TokensOut,
		})
	}
	return out, nil
}

const dateFmt = "2006-01-02"

// redFlags computes the per-community abuse signals and keeps only those scored
// above "low", worst first — the dashboard's "Red flags" panel. Everything here
// is aggregate metadata (counts/ratios/score), so it never exposes the tenant
// content the operator can no longer read in SaaS.
func (h *Handler) redFlags(ctx context.Context) ([]webtempl.SACommunityRisk, error) {
	risks, err := h.Communities.RiskSignals(ctx, time.Now())
	if err != nil {
		return nil, err
	}
	out := make([]webtempl.SACommunityRisk, 0, len(risks))
	for _, cr := range risks {
		if cr.Assessment.Band == community.RiskLow {
			continue // healthy — not a flag
		}
		out = append(out, webtempl.SACommunityRisk{
			Slug:          cr.Community.Slug,
			Name:          cr.Community.Name,
			Score:         cr.Assessment.Score,
			Band:          cr.Assessment.Band,
			Reasons:       cr.Assessment.Reasons,
			Categories:    categoryLabels(cr.Signals.FlaggedCategories),
			MembersTotal:  cr.Signals.MembersTotal,
			MembersNew24h: cr.Signals.MembersNew24h,
			Messages24h:   cr.Signals.Messages24h,
		})
	}
	return out, nil
}

// toSAModerationFlags maps raw flag rows to the audit-card view: formats the
// timestamp and turns the stored category CSV into human labels. No message
// body is involved — none is stored.
func toSAModerationFlags(in []moderation.FlagRow) []webtempl.SAModerationFlag {
	out := make([]webtempl.SAModerationFlag, 0, len(in))
	for _, f := range in {
		var codes []string
		for _, c := range strings.Split(f.Categories, ",") {
			if c = strings.TrimSpace(c); c != "" {
				codes = append(codes, c)
			}
		}
		out = append(out, webtempl.SAModerationFlag{
			CreatedAt:     time.Unix(f.CreatedAt, 0).Local().Format("Jan 2 15:04"),
			CommunityName: f.CommunityName,
			CommunitySlug: f.CommunitySlug,
			Channel:       f.ChannelID,
			AuthorEmail:   f.AuthorEmail,
			Categories:    categoryLabels(codes),
			MessageID:     f.MessageID,
		})
	}
	return out
}

// categoryLabels maps Llama Guard hazard codes (e.g. "S12") to their human
// labels for display in the Red flags panel, joined for one line. Empty in →
// "". This is where the moderation taxonomy is applied; community.RiskSignals
// stays code-only so it needn't depend on internal/moderation.
func categoryLabels(codes []string) string {
	if len(codes) == 0 {
		return ""
	}
	out := make([]string, 0, len(codes))
	for _, c := range codes {
		out = append(out, moderation.CategoryLabel(c))
	}
	return strings.Join(out, ", ")
}

func toSACommunities(in []community.CommunityStat) []webtempl.SACommunity {
	out := make([]webtempl.SACommunity, 0, len(in))
	for _, c := range in {
		out = append(out, webtempl.SACommunity{
			ID:           c.ID,
			Slug:         c.Slug,
			Name:         c.Name,
			IsPublic:     c.IsPublic,
			MemberCount:  c.MemberCount,
			MessageCount: c.MessageCount,
			ThreadCount:  c.ThreadCount,
			CreatedAt:    c.CreatedAt.Format(dateFmt),
		})
	}
	return out
}

func toSAUsers(in []auth.GlobalUser) []webtempl.SAUser {
	out := make([]webtempl.SAUser, 0, len(in))
	for _, u := range in {
		out = append(out, webtempl.SAUser{
			ID:          u.ID,
			Email:       u.Email,
			Status:      string(u.Status),
			Communities: u.CommunityCount,
			CreatedAt:   u.CreatedAt.Format(dateFmt),
		})
	}
	return out
}

type createSignals struct {
	Name  string `json:"sa_name"`
	Slug  string `json:"sa_slug"`
	Email string `json:"sa_email"`
}

// PostCreateCommunity spins up a new community and makes the named existing
// user its first admin. Mirrors admin.PostCreateCommunity but lives behind
// the platform-wide super-admin gate.
func (h *Handler) PostCreateCommunity(w http.ResponseWriter, r *http.Request) {
	var in createSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-cc-error", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)
	name := strings.TrimSpace(in.Name)
	slug := strings.ToLower(strings.TrimSpace(in.Slug))
	email := strings.TrimSpace(in.Email)
	if name == "" || slug == "" || email == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-cc-error", "Name, slug and first-admin email are required"))
		return
	}
	if !validSlug(slug) {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-cc-error", "Slug must contain only a-z, 0-9, '-'"))
		return
	}
	user, err := h.AuthRepo.UserByEmail(r.Context(), email)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-cc-error", "No user with that email"))
		return
	}
	// Provision (create + seed #general + seed first member) lives in one place.
	if _, err := h.Provision.Create(r.Context(), provision.Input{
		Slug: slug, Name: name, OwnerUserID: user.ID,
		DisplayName: localPart(user.Email), Role: auth.RoleAdmin,
	}); err != nil {
		if errors.Is(err, community.ErrSlugTaken) {
			_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-cc-error", "Slug already in use"))
			return
		}
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-cc-error", err.Error()))
		return
	}
	_ = sse.Redirect("/superadmin")
}

type deleteSignals struct {
	CID         string `json:"sa_cid"`
	ConfirmSlug string `json:"sa_confirm_slug"`
}

// PostDeleteCommunity permanently deletes a community AND cascades its
// content (see community.Repo.Delete — this is destructive, not a safe
// no-op). To prevent accidental nukes it requires the caller to type the
// community's slug back: the delete proceeds only when sa_confirm_slug
// exactly matches the target community's slug. The action is audit-logged.
func (h *Handler) PostDeleteCommunity(w http.ResponseWriter, r *http.Request) {
	var in deleteSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)
	cid := strings.TrimSpace(in.CID)
	if cid == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "No community selected"))
		return
	}
	c, err := h.Communities.ByID(r.Context(), cid)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "No such community"))
		return
	}
	// Server-side guard — never trust the client prompt. The typed slug must
	// match exactly, or we refuse without touching the database.
	if strings.TrimSpace(in.ConfirmSlug) != c.Slug {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result",
			"Delete cancelled — the typed slug did not match \""+c.Slug+"\". Nothing was deleted."))
		return
	}
	// Provision.Delete purges ALL of the community's data: upload blobs, the
	// cascaded DB rows, and the vector collection (see provision.Service.Delete).
	if err := h.Provision.Delete(r.Context(), cid); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "Delete failed: "+err.Error()))
		return
	}
	actor := "unknown"
	if id, ok := auth.FromContext(r.Context()); ok {
		actor = id.User.Email
	}
	if h.Log != nil {
		h.Log.Warn("super-admin deleted community (cascaded)",
			"actor", actor, "community_id", cid, "slug", c.Slug, "name", c.Name)
	}
	_ = sse.Redirect("/superadmin")
}

type uidSignals struct {
	UID string `json:"sa_uid"`
}

// PostDisableUser disables an account platform-wide. auth.Loader signs the
// user out on their next request. A super-admin can't disable themselves.
func (h *Handler) PostDisableUser(w http.ResponseWriter, r *http.Request) {
	h.setStatus(w, r, auth.StatusDisabled)
}

// PostEnableUser re-activates a disabled account.
func (h *Handler) PostEnableUser(w http.ResponseWriter, r *http.Request) {
	h.setStatus(w, r, auth.StatusActive)
}

func (h *Handler) setStatus(w http.ResponseWriter, r *http.Request, status auth.UserStatus) {
	var in uidSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)
	uid := strings.TrimSpace(in.UID)
	if uid == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "No user selected"))
		return
	}
	if status == auth.StatusDisabled {
		if id, ok := auth.FromContext(r.Context()); ok && id.User.ID == uid {
			_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "You can't disable your own account"))
			return
		}
	}
	if err := h.AuthRepo.SetUserStatus(r.Context(), uid, status); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "Update failed: "+err.Error()))
		return
	}
	_ = sse.Redirect("/superadmin")
}

// PostForceVerify activates a pending signup without the email round-trip — the
// operator escape hatch when verification mail can't reach the address at all.
func (h *Handler) PostForceVerify(w http.ResponseWriter, r *http.Request) {
	var in uidSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)
	uid := strings.TrimSpace(in.UID)
	if uid == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "No user selected"))
		return
	}
	if h.Auth == nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "Verification recovery is unavailable"))
		return
	}
	if err := h.Auth.ForceVerify(r.Context(), uid); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "Verify failed: "+err.Error()))
		return
	}
	_ = sse.Redirect("/superadmin")
}

// PostResendVerification re-sends the verification email to a pending signup and
// surfaces the verify URL so the operator can hand it over directly when mail
// delivery itself is broken.
func (h *Handler) PostResendVerification(w http.ResponseWriter, r *http.Request) {
	var in uidSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)
	uid := strings.TrimSpace(in.UID)
	if uid == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "No user selected"))
		return
	}
	if h.Auth == nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "Verification recovery is unavailable"))
		return
	}
	url, err := h.Auth.ResendVerification(r.Context(), uid)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "Resend failed: "+err.Error()))
		return
	}
	if url == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "User is not pending — nothing to resend."))
		return
	}
	_ = sse.PatchElementTempl(webtempl.SuccessFragment("sa-result",
		"Verification email re-sent. If mail still fails, give them this link: "+url))
}

func toSAUserMemberships(in []auth.UserMembership) []webtempl.SAUserMembership {
	now := time.Now()
	out := make([]webtempl.SAUserMembership, 0, len(in))
	for _, m := range in {
		row := webtempl.SAUserMembership{
			MembershipID: m.MembershipID,
			Slug:         m.Slug,
			Name:         m.Name,
			Role:         string(m.Role),
			IsApproved:   m.IsApproved,
			IsBanned:     m.BannedUntil != nil && m.BannedUntil.After(now),
			ChatCount:    m.ChatCount,
			ThreadCount:  m.ThreadCount,
		}
		if m.LastActive != nil {
			row.LastActive = m.LastActive.Format(dateFmt)
		}
		out = append(out, row)
	}
	return out
}

// renderMemberships re-renders the drill-down fragment for one user. Used by
// the expand GET and after every per-community action so the panel reflects
// the new state in place (live morph — keeps the row expanded, §4.7).
func (h *Handler) renderMemberships(sse *datastar.ServerSentEventGenerator, r *http.Request, uid string) {
	mems, err := h.AuthRepo.UserMemberships(r.Context(), uid)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "load memberships: "+err.Error()))
		return
	}
	_ = sse.PatchElementTempl(webtempl.SAUserMemberships(uid, toSAUserMemberships(mems)))
}

// GetUserMemberships lazily loads the per-user community drill-down: which
// communities the user belongs to, their role + state in each, basic activity
// counts, and per-community ban/remove actions. Answers "this user shows 2
// communities — which 2?".
func (h *Handler) GetUserMemberships(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	sse := render.NewSSE(w, r)
	if uid == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "No user selected"))
		return
	}
	h.renderMemberships(sse, r, uid)
}

type memActionSignals struct {
	MID      string `json:"sa_mid"`
	UID      string `json:"sa_uid"`
	BanHours int    `json:"ban_hours"`
	Role     string `json:"sa_role"`
}

// PostCommunityRole sets a user's role (member|moderator|admin) in one
// community, targeting the membership directly. This is the platform
// super-admin's way to make any email a community admin from the GUI without
// being a member of that community. Guard: demoting an admin is refused when
// they're the community's last one, so a community can't be orphaned (mirrors
// PostCommunityRemove). Re-renders the drill-down so the row flips live.
func (h *Handler) PostCommunityRole(w http.ResponseWriter, r *http.Request) {
	var in memActionSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)
	mid := strings.TrimSpace(in.MID)
	if mid == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "No membership selected"))
		return
	}
	role := auth.Role(strings.TrimSpace(in.Role))
	if role != auth.RoleMember && role != auth.RoleMod && role != auth.RoleAdmin && role != auth.RoleOwner {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "Invalid role"))
		return
	}
	m, err := h.AuthRepo.MembershipByID(r.Context(), mid)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "No such membership"))
		return
	}
	if m.Role == role {
		h.renderMemberships(sse, r, m.UserID) // no-op change, just re-sync the row
		return
	}
	// Demoting away from a privileged role (admin/owner) must not leave the
	// community without one.
	if m.Role.AtLeast(auth.RoleAdmin) && !role.AtLeast(auth.RoleAdmin) {
		count, err := h.AuthRepo.CountAdmins(r.Context(), m.CommunityID)
		if err != nil {
			_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "count admins: "+err.Error()))
			return
		}
		if count <= 1 {
			_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "Refused — this is the community's last admin. Promote another admin first."))
			return
		}
	}
	if err := h.AuthRepo.UpdateMembershipRole(r.Context(), mid, role); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "Role change failed: "+err.Error()))
		return
	}
	h.audit(r, "super-admin set community role", "membership_id", mid, "community_id", m.CommunityID, "user_id", m.UserID, "role", string(role))
	h.renderMemberships(sse, r, m.UserID)
}

// PostCommunityBan bans a user from one community (permanent unless ban_hours
// is set), targeting the membership directly. Re-renders the drill-down so the
// row flips to "banned" live.
func (h *Handler) PostCommunityBan(w http.ResponseWriter, r *http.Request) {
	var in memActionSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)
	mid := strings.TrimSpace(in.MID)
	if mid == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "No membership selected"))
		return
	}
	m, err := h.AuthRepo.MembershipByID(r.Context(), mid)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "No such membership"))
		return
	}
	// Don't let a ban orphan a community by removing its last admin/owner from
	// circulation (FIX1 L2) — a banned member can't administer it. Same guard as
	// remove; impact is limited (a super-admin can recover via /superadmin) but
	// the warning prevents the easy mistake.
	if m.Role.AtLeast(auth.RoleAdmin) {
		count, err := h.AuthRepo.CountAdmins(r.Context(), m.CommunityID)
		if err != nil {
			_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "count admins: "+err.Error()))
			return
		}
		if count <= 1 {
			_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "Refused — this is the community's last privileged member. Promote another admin/owner first."))
			return
		}
	}
	var until time.Time
	if in.BanHours <= 0 {
		until = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	} else {
		until = time.Now().Add(time.Duration(in.BanHours) * time.Hour)
	}
	if err := h.AuthRepo.UpdateBan(r.Context(), mid, &until); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "Ban failed: "+err.Error()))
		return
	}
	h.audit(r, "super-admin banned user from community", "membership_id", mid, "community_id", m.CommunityID, "user_id", m.UserID)
	h.renderMemberships(sse, r, m.UserID)
}

// PostCommunityRemove hard-deletes one membership row (the account is kept).
// Guarded so a community can't be orphaned by removing its last admin.
func (h *Handler) PostCommunityRemove(w http.ResponseWriter, r *http.Request) {
	var in memActionSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)
	mid := strings.TrimSpace(in.MID)
	if mid == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "No membership selected"))
		return
	}
	m, err := h.AuthRepo.MembershipByID(r.Context(), mid)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "No such membership"))
		return
	}
	// Atomic last-admin guard (FIX1 M2) — same race fix as the per-community
	// admin path: refuse and delete in one statement instead of CountAdmins→Reject.
	deleted, err := h.AuthRepo.DeleteMembershipIfNotLastAdmin(r.Context(), mid, m.CommunityID)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "Remove failed: "+err.Error()))
		return
	}
	if !deleted {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "Refused — this is the community's last privileged member. Promote another admin/owner first."))
		return
	}
	h.audit(r, "super-admin removed user from community", "membership_id", mid, "community_id", m.CommunityID, "user_id", m.UserID)
	h.renderMemberships(sse, r, m.UserID)
}

// PostSystemBan is the platform-wide kill switch: disables the account (the
// user is signed out on their next request) AND soft-deletes all their chat,
// threads and posts across every community they belong to. A super-admin
// can't system-ban themselves.
func (h *Handler) PostSystemBan(w http.ResponseWriter, r *http.Request) {
	var in uidSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)
	uid := strings.TrimSpace(in.UID)
	if uid == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "No user selected"))
		return
	}
	if id, ok := auth.FromContext(r.Context()); ok && id.User.ID == uid {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "You can't system-ban your own account"))
		return
	}
	if err := h.AuthRepo.SetUserStatus(r.Context(), uid, auth.StatusDisabled); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "Disable failed: "+err.Error()))
		return
	}
	mems, err := h.AuthRepo.UserMemberships(r.Context(), uid)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "Account disabled, but loading communities to wipe failed: "+err.Error()))
		return
	}
	opts := auth.CleanupOptions{Chat: true, Threads: true, Posts: true}
	for _, m := range mems {
		if err := h.AuthRepo.CleanupUserContent(r.Context(), uid, m.CommunityID, opts); err != nil && h.Log != nil {
			h.Log.Error("system-ban cleanup", "err", err, "user_id", uid, "community_id", m.CommunityID)
		}
	}
	if h.Bus != nil {
		h.Bus.Broadcast("")
	}
	h.audit(r, "super-admin system-banned user (disabled + content wiped)", "user_id", uid, "communities", len(mems))
	_ = sse.Redirect("/superadmin")
}

// audit logs a destructive super-admin action with the acting account's email.
// PostReindexAll drops the whole vector index and re-queues every community's
// public content. The embed worker processes the queue in the background; the
// returned count is the resulting queue depth.
func (h *Handler) PostReindexAll(w http.ResponseWriter, r *http.Request) {
	sse := datastar.NewSSE(w, r)
	if h.RAG == nil {
		_ = sse.PatchElementTempl(webtempl.SAReindexResult("RAG is disabled on this instance."))
		return
	}
	n, err := h.RAG.ReindexAll(r.Context())
	if err != nil {
		h.audit(r, "reindex all failed", "err", err)
		_ = sse.PatchElementTempl(webtempl.SAReindexResult("Reindex failed: " + err.Error()))
		return
	}
	h.audit(r, "reindex all queued", "jobs", n)
	_ = sse.PatchElementTempl(webtempl.SAReindexResult(
		fmt.Sprintf("Reindex queued — %d jobs. The embed worker is processing them in the background.", n)))
}

type broadcastSignals struct {
	Message string `json:"sa_broadcast"`
}

// PostBroadcast posts the super-admin's announcement as a system message into
// EVERY community's #general and fans each out live, so it appears in all open
// chats immediately. The text is rendered through the user-markdown pipeline
// (sanitised) and prefixed with a 📢 banner. Best-effort per community: a
// failure on one is logged and the rest still go out.
func (h *Handler) PostBroadcast(w http.ResponseWriter, r *http.Request) {
	var in broadcastSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-broadcast-result", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)
	msg := strings.TrimSpace(in.Message)
	if msg == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-broadcast-result", "Message is required"))
		return
	}
	if h.Chat == nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-broadcast-result", "Chat broadcast is unavailable on this instance"))
		return
	}
	body, err := render.RenderMarkdown(msg)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-broadcast-result", "render message: "+err.Error()))
		return
	}
	html := `<p>📢 <strong>Platform broadcast</strong></p>` + body
	comms, err := h.Communities.ListAll(r.Context())
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-broadcast-result", "load communities: "+err.Error()))
		return
	}
	var sent, failed int
	for _, c := range comms {
		if err := h.Chat.SystemBroadcast(r.Context(), c.ID, html); err != nil {
			failed++
			if h.Log != nil {
				h.Log.Error("super-admin broadcast", "err", err, "community_id", c.ID)
			}
			continue
		}
		sent++
	}
	h.audit(r, "super-admin broadcast to all communities", "sent", sent, "failed", failed)
	_ = sse.PatchSignals([]byte(`{"sa_broadcast":""}`))
	_ = sse.PatchElementTempl(webtempl.SABroadcastResult(broadcastSummary(sent, failed)))
}

// broadcastSummary phrases the broadcast outcome for the result fragment.
func broadcastSummary(sent, failed int) string {
	out := fmt.Sprintf("📢 Sent to %d communit%s.", sent, plural(sent))
	if failed > 0 {
		out += fmt.Sprintf(" %d failed (see logs).", failed)
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

func (h *Handler) audit(r *http.Request, msg string, kv ...any) {
	if h.Log == nil {
		return
	}
	actor := "unknown"
	if id, ok := auth.FromContext(r.Context()); ok {
		actor = id.User.Email
	}
	h.Log.Warn(msg, append([]any{"actor", actor}, kv...)...)
}

// ----- Debug log (in-memory switch + DB capture) ----------------------------

// PostDebugToggle flips the in-memory debug-recording switch from the ?on=1|0
// query param and morphs the shared #sa-debug-card so the new state shows live
// on whichever surface fired it (dashboard or the debug page).
func (h *Handler) PostDebugToggle(w http.ResponseWriter, r *http.Request) {
	on := r.URL.Query().Get("on") == "1"
	h.Debug.SetEnabled(on)
	h.audit(r, "super-admin toggled debug recording", "enabled", on)
	count, _ := h.Debug.Count(r.Context())
	sse := render.NewSSE(w, r)
	_ = sse.PatchElementTempl(webtempl.SADebugCard(h.Debug.Enabled(), count))
}

// GetDebug renders the platform debug-log viewer: the on/off card plus the
// captured payloads, newest first.
func (h *Handler) GetDebug(w http.ResponseWriter, r *http.Request) {
	entries, err := h.Debug.List(r.Context())
	if err != nil {
		http.Error(w, "load debug log: "+err.Error(), http.StatusInternalServerError)
		return
	}
	data := webtempl.SADebugPageData{
		Viewer:  h.viewer(r),
		Enabled: h.Debug.Enabled(),
		Count:   len(entries),
		Entries: toSADebugEntries(entries),
	}
	_ = webtempl.SADebugPage(data).Render(r.Context(), w)
}

// PostDebugClear deletes every captured entry and re-renders the (now empty)
// list plus the card count. The switch itself is left as-is.
func (h *Handler) PostDebugClear(w http.ResponseWriter, r *http.Request) {
	sse := render.NewSSE(w, r)
	if err := h.Debug.Clear(r.Context()); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-debug-list", "Clear failed: "+err.Error()))
		return
	}
	h.audit(r, "super-admin cleared debug log")
	_ = sse.PatchElementTempl(webtempl.SADebugCard(h.Debug.Enabled(), 0))
	_ = sse.PatchElementTempl(webtempl.SADebugList(nil))
}

func toSADebugEntries(in []debuglog.Entry) []webtempl.SADebugEntry {
	out := make([]webtempl.SADebugEntry, 0, len(in))
	for _, e := range in {
		out = append(out, webtempl.SADebugEntry{
			ID:        e.ID,
			CreatedAt: e.CreatedAt.Local().Format("Jan 2 15:04:05"),
			Source:    e.Source,
			Event:     e.Event,
			Summary:   e.Summary,
			Payload:   e.Payload,
			Meta:      e.Meta,
		})
	}
	return out
}
