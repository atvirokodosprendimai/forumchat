package dashboard

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/provision"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

var slugRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// communityHoldMsg reports whether the user's account is too new to create or
// request a community (< NewUserCommunityDelay old), with a human "try again
// in N" message. Keyed off account age so established users are never blocked.
func communityHoldMsg(u auth.User) (string, bool) {
	remaining := auth.NewUserCommunityDelay - u.Age(time.Now())
	if remaining <= 0 {
		return "", false
	}
	if mins := int(remaining.Minutes()); mins > 0 {
		return fmt.Sprintf("New accounts can create a community a few minutes after signing up — try again in about %dm%02ds.", mins, int(remaining.Seconds())%60), true
	}
	return fmt.Sprintf("New accounts can create a community a few minutes after signing up — try again in %ds.", int(remaining.Seconds())+1), true
}

func localPart(email string) string {
	if i := strings.IndexByte(email, '@'); i > 0 {
		return email[:i]
	}
	return email
}

type createSignals struct {
	Name string `json:"nc_name"`
	Slug string `json:"nc_slug"`
}

type requestSignals struct {
	Name   string `json:"cr_name"`
	Slug   string `json:"cr_slug"`
	Reason string `json:"cr_reason"`
}

// PostCreate is the SaaS self-serve create: a user who owns no community spins
// one up instantly and becomes its owner. Over-quota users are routed to the
// request flow instead — the UI hides this form, and the server re-checks the
// quota so a crafted request can't bypass it.
func (h *Handler) PostCreate(w http.ResponseWriter, r *http.Request) {
	if !h.Cfg.SAAS {
		http.NotFound(w, r)
		return
	}
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var in createSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("nc-error", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)
	name := strings.TrimSpace(in.Name)
	slug := strings.ToLower(strings.TrimSpace(in.Slug))
	if name == "" || slug == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("nc-error", "Name and slug are required"))
		return
	}
	if !slugRE.MatchString(slug) {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("nc-error", "Slug must contain only a-z, 0-9, '-'"))
		return
	}
	if msg, blocked := communityHoldMsg(id.User); blocked {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("nc-error", msg))
		return
	}
	owned, err := h.Auth.CountOwnedByUser(r.Context(), id.User.ID)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("nc-error", "Could not verify your communities; try again"))
		return
	}
	if owned > 0 {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("nc-error", "You already own a community — request another below."))
		return
	}
	c, err := h.Provision.Create(r.Context(), provision.Input{
		Slug: slug, Name: name, OwnerUserID: id.User.ID,
		DisplayName: localPart(id.User.Email), Role: auth.RoleOwner,
	})
	if err != nil {
		if errors.Is(err, community.ErrSlugTaken) {
			_ = sse.PatchElementTempl(webtempl.ErrorFragment("nc-error", "That slug is taken — pick another"))
			return
		}
		h.Log.Error("self-serve community create", "user", id.User.ID, "slug", slug, "err", err)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("nc-error", "Could not create the community; try again"))
		return
	}
	_ = sse.Redirect("/c/" + c.Slug + "/chat")
}

// PostRequest queues an over-quota user's request for an additional community,
// pending platform super-admin approval. One pending request per user.
func (h *Handler) PostRequest(w http.ResponseWriter, r *http.Request) {
	if !h.Cfg.SAAS {
		http.NotFound(w, r)
		return
	}
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var in requestSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("cr-error", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)
	name := strings.TrimSpace(in.Name)
	slug := strings.ToLower(strings.TrimSpace(in.Slug))
	reason := strings.TrimSpace(in.Reason)
	if name == "" || slug == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("cr-error", "Name and slug are required"))
		return
	}
	if !slugRE.MatchString(slug) {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("cr-error", "Slug must contain only a-z, 0-9, '-'"))
		return
	}
	if msg, blocked := communityHoldMsg(id.User); blocked {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("cr-error", msg))
		return
	}
	if pending, err := h.Communities.CountPendingRequestsForUser(r.Context(), id.User.ID); err == nil && pending > 0 {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("cr-error", "You already have a request awaiting approval."))
		return
	}
	// Reject a slug that's already a live community early, so the requester fixes
	// it now rather than the super-admin hitting the clash at approval time.
	if _, err := h.Communities.BySlug(r.Context(), slug); err == nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("cr-error", "That slug is taken — pick another"))
		return
	}
	req, err := h.Communities.CreateRequest(r.Context(), id.User.ID, name, slug, reason)
	if err != nil {
		h.Log.Error("community request", "user", id.User.ID, "slug", slug, "err", err)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("cr-error", "Could not submit your request; try again"))
		return
	}
	// Morph the card to its pending state (§4.7 stable-id swap).
	_ = sse.PatchElementTempl(webtempl.DashboardCreateCard(webtempl.DashboardCreate{
		SaaS: true, HasPending: true, PendingSlug: req.Slug,
	}))
}
