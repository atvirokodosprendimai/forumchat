// Package support implements the hidden cross-tenant "support inbox":
// ONE designated community (config SUPPORT_INBOX_SLUG) acts as a
// write-only issue inbox. Any signed-in member files a report from the
// global "Report issue" button; the report lands as a project_issue in
// the support community's "Inbox" project. Reporters never become
// members of that community, so they can only read back their OWN
// reports (and replies) through the handlers here — they cannot browse
// the inbox. Only platform super-admins read the full inbox, via the
// existing god-mode path /c/<slug>/projects/<inbox>/issues.
//
// A report is a projects.Issue; a reply is a projects.IssueComment — the
// whole issue machinery is reused, no new domain tables.
package support

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/projects"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// inboxTitle is the project that holds every report. find-or-create by
// title (see projects.Service.EnsureNamedProject).
const inboxTitle = "Inbox"

const inboxDesc = "Cross-tenant support reports filed from the global “Report issue” button. " +
	"Only platform super-admins read this."

// Handler serves the global /report-issue surface. It is bound at
// construction to the single resolved support community.
type Handler struct {
	Community *community.Repo
	Issues    *projects.Service
	Projects  *projects.Repo
	Log       *slog.Logger

	communityID string // resolved support community id (immutable)
}

// New binds a support handler to the already-resolved support community.
func New(communityID string, comm *community.Repo, issues *projects.Service, projs *projects.Repo, log *slog.Logger) *Handler {
	return &Handler{Community: comm, Issues: issues, Projects: projs, Log: log, communityID: communityID}
}

// caller derives the issue-author identity from the auth context. ok is
// false for unauthenticated requests. The display name falls back to the
// email so a report is never authored by a blank name.
func caller(r *http.Request) (projects.Identity, auth.Identity, bool) {
	aid, ok := auth.FromContext(r.Context())
	if !ok || aid.User.ID == "" {
		return projects.Identity{}, auth.Identity{}, false
	}
	name := strings.TrimSpace(aid.Membership.DisplayName)
	if name == "" {
		name = aid.User.Email
	}
	return projects.Identity{UserID: aid.User.ID, Name: name, Role: aid.Membership.Role}, aid, true
}

// GetReport renders the report composer + the caller's own past reports.
func (h *Handler) GetReport(w http.ResponseWriter, r *http.Request) {
	id, _, ok := caller(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	_ = webtempl.SupportReportPage(webtempl.SupportReportPageData{
		Viewer:  h.viewer(r),
		Reports: h.myReports(r.Context(), id.UserID),
	}).Render(r.Context(), w)
}

type reportIn struct {
	Title string `json:"report_title"`
	Body  string `json:"report_body"`
}

// PostReport creates a new report in the support inbox, authored by the
// caller, stamped with their home-community context for triage.
func (h *Handler) PostReport(w http.ResponseWriter, r *http.Request) {
	var in reportIn
	if err := datastar.ReadSignals(r, &in); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "bad signals", http.StatusBadRequest)
		return
	}
	id, aid, ok := caller(r)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}

	sse := render.NewSSE(w, r)
	title := strings.TrimSpace(in.Title)
	if title == "" {
		h.flash(sse, "Please add a subject.", true)
		return
	}
	// Create-on-write: the first report mints the Inbox project (credited
	// to the reporter — harmless, they can't reach the project surface).
	pid, err := h.Issues.EnsureNamedProject(r.Context(), h.communityID, id.UserID, inboxTitle, inboxDesc)
	if err != nil {
		h.Log.Error("support: ensure inbox project", "err", err)
		h.flash(sse, "Something went wrong. Please try again.", true)
		return
	}
	if _, err := h.Issues.CreateIssue(r.Context(), pid, title, h.composeBody(r.Context(), id, aid, in.Body), id); err != nil {
		h.Log.Error("support: create report", "err", err)
		h.flash(sse, "Couldn’t send your report. Please try again.", true)
		return
	}
	_ = sse.PatchSignals([]byte(`{"report_title":"","report_body":""}`))
	_ = sse.PatchElementTempl(webtempl.SupportMyReports(h.myReports(r.Context(), id.UserID)), datastar.WithModeOuter())
	h.flash(sse, "Thanks — your report was sent. We’ll reply here.", false)
}

// GetReportDetail renders one report + its thread. A reporter sees only
// their own (anti-enumeration: a not-owned or unknown id is a 404, never
// a 403); a super-admin sees any report and gets the status control.
func (h *Handler) GetReportDetail(w http.ResponseWriter, r *http.Request) {
	id, aid, ok := caller(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	issue, ok := h.accessibleIssue(r.Context(), chi.URLParam(r, "iid"), id.UserID, aid.IsSuperAdmin)
	if !ok {
		http.NotFound(w, r)
		return
	}
	comments, err := h.Projects.ListIssueComments(r.Context(), issue.ID)
	if err != nil {
		h.Log.Error("support: list comments", "err", err)
	}
	_ = webtempl.SupportReportDetailPage(webtempl.SupportReportDetailData{
		Viewer:   h.viewer(r),
		Report:   toReportRow(issue),
		BodyHTML: issue.BodyHTML,
		Comments: toCommentViews(comments, id.UserID),
		IsAdmin:  aid.IsSuperAdmin,
	}).Render(r.Context(), w)
}

// GetInbox is the super-admin triage view: ALL reports across every
// tenant, newest activity first. Self-contained — does NOT depend on
// PROJECTS_ENABLED or the per-community projects UI. Route is gated by
// auth.RequireSuperAdmin.
func (h *Handler) GetInbox(w http.ResponseWriter, r *http.Request) {
	pid, err := h.findInboxProjectID(r.Context())
	if err != nil {
		h.Log.Error("support: inbox find project", "err", err)
	}
	var rows []webtempl.SupportReportRow
	if pid != "" {
		issues, err := h.Projects.ListIssues(r.Context(), pid, true) // includeClosed
		if err != nil {
			h.Log.Error("support: inbox list", "err", err)
		}
		rows = toAdminReportRows(issues)
	}
	_ = webtempl.SupportInboxPage(webtempl.SupportInboxPageData{
		Viewer:  h.viewer(r),
		Reports: rows,
	}).Render(r.Context(), w)
}

type statusIn struct {
	Status string `json:"report_status"`
}

// PostStatus changes a report's status. Super-admin only (route gated by
// auth.RequireSuperAdmin); the status arrives as a ?to= query param.
func (h *Handler) PostStatus(w http.ResponseWriter, r *http.Request) {
	var in statusIn
	_ = datastar.ReadSignals(r, &in) // tolerate empty body; we use the query param
	id, aid, ok := caller(r)
	if !ok || !aid.IsSuperAdmin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	status := r.URL.Query().Get("to")
	issue, owned := h.accessibleIssue(r.Context(), chi.URLParam(r, "iid"), id.UserID, true)

	sse := render.NewSSE(w, r)
	if !owned {
		_ = sse.Redirect("/support-inbox")
		return
	}
	if err := h.Issues.UpdateIssueStatus(r.Context(), issue.ProjectID, issue.ID, status, id); err != nil {
		h.Log.Error("support: status", "err", err, "to", status)
		return
	}
	fresh, err := h.Projects.IssueByID(r.Context(), issue.ID)
	if err != nil {
		return
	}
	_ = sse.PatchElementTempl(webtempl.SupportStatusBar(toReportRow(fresh), true), datastar.WithModeOuter())
}

type replyIn struct {
	Body string `json:"report_reply"`
}

// PostReply appends the caller's reply to their own report's thread.
func (h *Handler) PostReply(w http.ResponseWriter, r *http.Request) {
	var in replyIn
	if err := datastar.ReadSignals(r, &in); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "bad signals", http.StatusBadRequest)
		return
	}
	id, aid, ok := caller(r)
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	issue, owned := h.accessibleIssue(r.Context(), chi.URLParam(r, "iid"), id.UserID, aid.IsSuperAdmin)

	sse := render.NewSSE(w, r)
	if !owned {
		_ = sse.Redirect("/report-issue")
		return
	}
	if strings.TrimSpace(in.Body) == "" {
		return
	}
	if _, err := h.Issues.AddIssueComment(r.Context(), issue.ProjectID, issue.ID, id, in.Body); err != nil {
		h.Log.Error("support: add reply", "err", err)
		return
	}
	comments, _ := h.Projects.ListIssueComments(r.Context(), issue.ID)
	_ = sse.PatchSignals([]byte(`{"report_reply":""}`))
	_ = sse.PatchElementTempl(webtempl.SupportThread(toCommentViews(comments, id.UserID)), datastar.WithModeOuter())
}

// ----- internals ---------------------------------------------------------

// accessibleIssue loads an issue and asserts it is in the support Inbox
// project AND the caller may see it: a super-admin sees every report; any
// other user sees only their own. Either failing reads as not-found — the
// load-bearing guard that keeps the inbox write-only for reporters.
func (h *Handler) accessibleIssue(ctx context.Context, iid, userID string, isSuperAdmin bool) (projects.Issue, bool) {
	if iid == "" {
		return projects.Issue{}, false
	}
	issue, err := h.Projects.IssueByID(ctx, iid)
	if err != nil {
		return projects.Issue{}, false
	}
	pid, err := h.findInboxProjectID(ctx)
	if err != nil || pid == "" || issue.ProjectID != pid {
		return projects.Issue{}, false
	}
	if isSuperAdmin {
		return issue, true
	}
	if userID == "" || issue.CreatorUserID != userID {
		return projects.Issue{}, false
	}
	return issue, true
}

// findInboxProjectID returns the Inbox project id, or "" when no report
// has ever been filed (the project is created lazily on first report).
// Read-only — never creates (queries must not write, §6b).
func (h *Handler) findInboxProjectID(ctx context.Context) (string, error) {
	rows, err := h.Projects.ListActiveForCommunity(ctx, h.communityID)
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		if strings.EqualFold(row.Title, inboxTitle) {
			return row.ID, nil
		}
	}
	return "", nil
}

// myReports lists the caller's own reports, newest first. Empty (not an
// error) before the Inbox project exists.
func (h *Handler) myReports(ctx context.Context, userID string) []webtempl.SupportReportRow {
	pid, err := h.findInboxProjectID(ctx)
	if err != nil {
		h.Log.Error("support: find inbox", "err", err)
		return nil
	}
	if pid == "" {
		return nil
	}
	issues, err := h.Projects.IssuesByCreator(ctx, pid, userID)
	if err != nil {
		h.Log.Error("support: issues by creator", "err", err)
		return nil
	}
	return toReportRows(issues)
}

// composeBody prepends a triage header (reporter name, email, home
// community) so a super-admin reading the inbox knows which tenant the
// report came from. The reporter is never told who reads it.
func (h *Handler) composeBody(ctx context.Context, id projects.Identity, aid auth.Identity, body string) string {
	body = strings.TrimSpace(body)
	var b strings.Builder
	b.WriteString("> **Reporter:** ")
	b.WriteString(id.Name)
	if aid.User.Email != "" {
		b.WriteString(" · ")
		b.WriteString(aid.User.Email)
	}
	if c, err := h.Community.ByID(ctx, aid.Membership.CommunityID); err == nil && c.Name != "" {
		b.WriteString("\n>\n> **From community:** ")
		b.WriteString(c.Name)
		b.WriteString(" (")
		b.WriteString(c.Slug)
		b.WriteString(")")
	}
	b.WriteString("\n\n")
	b.WriteString(body)
	return b.String()
}

// flash patches the #support-flash banner (ok or error styling).
func (h *Handler) flash(sse *datastar.ServerSentEventGenerator, msg string, isErr bool) {
	_ = sse.PatchElementTempl(webtempl.SupportFlash(msg, isErr), datastar.WithModeOuter())
}

// viewer builds the layout Viewer for a global (non-community) page —
// mirrors projects.Handler.layoutViewer.
func (h *Handler) viewer(r *http.Request) webtempl.Viewer {
	v := webtempl.Viewer{}
	if id, ok := auth.FromContext(r.Context()); ok {
		v.IsAuthed = true
		v.DisplayName = id.Membership.DisplayName
		v.Role = string(id.Membership.Role)
	}
	return v
}

func toReportRows(issues []projects.Issue) []webtempl.SupportReportRow {
	out := make([]webtempl.SupportReportRow, 0, len(issues))
	for _, i := range issues {
		out = append(out, toReportRow(i))
	}
	return out
}

func toReportRow(i projects.Issue) webtempl.SupportReportRow {
	return webtempl.SupportReportRow{
		IssueID:       i.ID,
		Title:         i.Title,
		Status:        i.Status,
		CreatedAtUnix: i.CreatedAt.Unix(),
		UpdatedAtUnix: i.UpdatedAt.Unix(),
	}
}

// toAdminReportRows is the super-admin inbox mapping: same rows as the
// reporter view but each carries the reporter's name (Reporter) so triage
// shows who filed it.
func toAdminReportRows(issues []projects.Issue) []webtempl.SupportReportRow {
	out := make([]webtempl.SupportReportRow, 0, len(issues))
	for _, i := range issues {
		row := toReportRow(i)
		row.Reporter = i.CreatorName
		out = append(out, row)
	}
	return out
}

// toCommentViews maps non-deleted comments to the view model, flagging
// which ones the caller authored ("You").
func toCommentViews(cs []projects.IssueComment, meID string) []webtempl.SupportCommentView {
	out := make([]webtempl.SupportCommentView, 0, len(cs))
	for _, c := range cs {
		if c.IsDeleted() {
			continue
		}
		out = append(out, webtempl.SupportCommentView{
			AuthorName:    c.AuthorName,
			BodyHTML:      c.BodyHTML,
			CreatedAtUnix: c.CreatedAt.Unix(),
			IsMine:        meID != "" && c.AuthorUserID == meID,
		})
	}
	return out
}
