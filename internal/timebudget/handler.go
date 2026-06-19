package timebudget

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

type Handler struct {
	Repo *Repo
	// ListProjects returns the community's active projects for the entry
	// form's optional "tag a task" <select>. Returns nil when the Projects
	// feature is disabled. A closure to avoid a projects → timebudget import
	// cycle (wired in main.go, same trick as chat.ListProjects).
	ListProjects func(ctx context.Context, communityID string) []webtempl.TimeProjectView
	Log          *slog.Logger
}

func (h *Handler) viewer(r *http.Request) webtempl.Viewer {
	v := webtempl.Viewer{}
	if c, ok := community.FromContext(r.Context()); ok {
		v.CommunityName = c.Name
		v.CommunitySlug = c.Slug
	}
	if id, ok := auth.FromContext(r.Context()); ok {
		v.IsAuthed = true
		v.DisplayName = id.Membership.DisplayName
		v.Role = string(id.Membership.Role)
	}
	return v
}

// GetPage renders the budget page for the current community. Any approved
// member can view; write affordances render only for moderators/admins.
func (h *Handler) GetPage(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	c := community.MustFromContext(r.Context())
	panel, err := h.panel(r.Context(), c.Slug, c.ID, id, monthOf(r))
	if err != nil {
		http.Error(w, "load budget: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_ = webtempl.BudgetPage(webtempl.TimeBudgetPageData{
		Viewer: h.viewer(r),
		Panel:  panel,
	}).Render(r.Context(), w)
}

// PostSetBudget sets the recurring monthly budget. Admin (or super-admin) only.
func (h *Handler) PostSetBudget(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	if !(id.IsSuperAdmin || id.Membership.Role.AtLeast(auth.RoleAdmin)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	c := community.MustFromContext(r.Context())
	var in struct {
		Hours string `json:"tb_budget_hours"`
	}
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	minutes := hoursToMinutes(in.Hours)
	if minutes < 0 {
		minutes = 0
	}
	if err := h.Repo.SetBudget(r.Context(), c.ID, minutes, id.User.ID); err != nil {
		h.Log.Error("set budget", "err", err)
		return
	}
	h.patchPanel(w, r, id, c.Slug, c.ID, monthOf(r), false)
}

// PostAddEntry logs a manual time entry. Moderator/admin only (route-gated).
func (h *Handler) PostAddEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	c := community.MustFromContext(r.Context())
	var in struct {
		Hours   string `json:"tb_hours"`
		Minutes string `json:"tb_minutes"`
		Note    string `json:"tb_note"`
		Date    string `json:"tb_date"`
		Project string `json:"tb_project"`
	}
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	total := atoiSafe(in.Hours)*60 + atoiSafe(in.Minutes)
	if total <= 0 {
		// Nothing to log — just clear the form so the user sees the reset.
		h.patchPanel(w, r, id, c.Slug, c.ID, monthOf(r), true)
		return
	}
	day := normalizeDay(in.Date)
	if _, err := h.Repo.AddEntry(r.Context(), Entry{
		CommunityID: c.ID,
		ProjectID:   strings.TrimSpace(in.Project),
		Minutes:     total,
		Note:        strings.TrimSpace(in.Note),
		OccurredOn:  day,
		CreatedBy:   id.User.ID,
	}); err != nil {
		h.Log.Error("add time entry", "err", err)
		return
	}
	// Re-render the month the new entry lands in, so it's visible even if the
	// user typed a date outside the month they were viewing.
	h.patchPanel(w, r, id, c.Slug, c.ID, day[:7], true)
}

// PostDeleteEntry removes an entry. Moderators/admins remove any; a non-mod
// can only remove their own (the route is mod-gated today, but the repo guard
// keeps the contract honest).
func (h *Handler) PostDeleteEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	c := community.MustFromContext(r.Context())
	entryID := chi.URLParam(r, "id")
	all := id.IsSuperAdmin || id.Membership.Role.AtLeast(auth.RoleMod)
	if err := h.Repo.DeleteEntry(r.Context(), c.ID, entryID, id.User.ID, all); err != nil {
		h.Log.Error("delete time entry", "err", err)
	}
	h.patchPanel(w, r, id, c.Slug, c.ID, monthOf(r), false)
}

// patchPanel re-renders #budget-panel (outer-morph) for the given month,
// optionally clearing the entry-form signals first.
func (h *Handler) patchPanel(w http.ResponseWriter, r *http.Request, id auth.Identity, slug, communityID, ym string, clearForm bool) {
	panel, err := h.panel(r.Context(), slug, communityID, id, ym)
	if err != nil {
		h.Log.Error("budget panel", "err", err)
		return
	}
	sse := render.NewSSE(w, r)
	if clearForm {
		_ = sse.PatchSignals([]byte(`{"tb_hours":"","tb_minutes":"","tb_note":"","tb_project":""}`))
	}
	_ = sse.PatchElementTempl(webtempl.BudgetPanel(panel), datastar.WithModeOuter())
}

// panel assembles the full view model for #budget-panel.
func (h *Handler) panel(ctx context.Context, slug, communityID string, id auth.Identity, ym string) (webtempl.TimeBudgetPanel, error) {
	b, err := h.Repo.GetBudget(ctx, communityID)
	if err != nil {
		return webtempl.TimeBudgetPanel{}, err
	}
	used, err := h.Repo.UsedMinutes(ctx, communityID, ym)
	if err != nil {
		return webtempl.TimeBudgetPanel{}, err
	}
	rows, err := h.Repo.ListEntries(ctx, communityID, ym)
	if err != nil {
		return webtempl.TimeBudgetPanel{}, err
	}

	canWrite := id.IsSuperAdmin || id.Membership.Role.AtLeast(auth.RoleMod)
	canSetBudget := id.IsSuperAdmin || id.Membership.Role.AtLeast(auth.RoleAdmin)

	groups := groupByProject(rows, id.User.ID, canWrite)

	var projects []webtempl.TimeProjectView
	if canWrite && h.ListProjects != nil {
		projects = h.ListProjects(ctx, communityID)
	}

	pct := 0
	if b.MonthlyMinutes > 0 {
		pct = used * 100 / b.MonthlyMinutes
		if pct > 100 {
			pct = 100
		}
	}
	return webtempl.TimeBudgetPanel{
		Slug:             slug,
		Month:            ym,
		MonthLabel:       monthLabel(ym),
		PrevMonth:        shiftMonth(ym, -1),
		NextMonth:        shiftMonth(ym, 1),
		BudgetMinutes:    b.MonthlyMinutes,
		UsedMinutes:      used,
		RemainingMinutes: b.MonthlyMinutes - used,
		OverBudget:       b.MonthlyMinutes > 0 && used > b.MonthlyMinutes,
		HasBudget:        b.MonthlyMinutes > 0,
		PctUsed:          pct,
		Groups:           groups,
		Projects:         projects,
		CanWrite:         canWrite,
		CanSetBudget:     canSetBudget,
		Today:            time.Now().Format("2006-01-02"),
	}, nil
}

// groupByProject buckets entries by project title (untagged last), summing
// minutes per group. Groups sort by minutes descending.
func groupByProject(rows []EntryRow, userID string, canWrite bool) []webtempl.TimeBudgetGroup {
	type acc struct {
		title   string
		minutes int
		entries []webtempl.TimeEntryView
	}
	order := []string{}
	byKey := map[string]*acc{}
	for _, row := range rows {
		key := row.ProjectID
		title := row.ProjectTitle
		if key == "" || title == "" {
			key, title = "", "Untagged"
		}
		a := byKey[key]
		if a == nil {
			a = &acc{title: title}
			byKey[key] = a
			order = append(order, key)
		}
		a.minutes += row.Minutes
		a.entries = append(a.entries, webtempl.TimeEntryView{
			ID:          row.ID,
			Note:        row.Note,
			Minutes:     row.Minutes,
			OccurredOn:  row.OccurredOn,
			CreatorName: row.CreatorName,
			CanDelete:   canWrite || row.CreatedBy == userID,
		})
	}
	out := make([]webtempl.TimeBudgetGroup, 0, len(order))
	for _, key := range order {
		a := byKey[key]
		out = append(out, webtempl.TimeBudgetGroup{
			ProjectTitle: a.title,
			Untagged:     key == "",
			Minutes:      a.minutes,
			Entries:      a.entries,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		// Untagged always sinks to the bottom; otherwise by minutes desc.
		if out[i].Untagged != out[j].Untagged {
			return !out[i].Untagged
		}
		return out[i].Minutes > out[j].Minutes
	})
	return out
}

// monthOf reads ?month=YYYY-MM, validating it; defaults to the current month.
func monthOf(r *http.Request) string {
	ym := strings.TrimSpace(r.URL.Query().Get("month"))
	if _, err := time.Parse("2006-01", ym); err == nil {
		return ym
	}
	return time.Now().Format("2006-01")
}

func monthLabel(ym string) string {
	t, err := time.Parse("2006-01", ym)
	if err != nil {
		return ym
	}
	return t.Format("January 2006")
}

func shiftMonth(ym string, delta int) string {
	t, err := time.Parse("2006-01", ym)
	if err != nil {
		return ym
	}
	return t.AddDate(0, delta, 0).Format("2006-01")
}

// normalizeDay validates a YYYY-MM-DD date, falling back to today.
func normalizeDay(s string) string {
	s = strings.TrimSpace(s)
	if _, err := time.Parse("2006-01-02", s); err == nil {
		return s
	}
	return time.Now().Format("2006-01-02")
}

// hoursToMinutes parses a decimal-hours string ("50", "1.5") into minutes.
func hoursToMinutes(s string) int {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return int(f*60 + 0.5)
}

func atoiSafe(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}
