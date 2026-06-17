package projects

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
)

type extractFromChatSignals struct {
	AttachmentID string `json:"extract_attachment_id"`
	ProjectID    string `json:"extract_project_id"`
	Mode         string `json:"extract_mode"`
	Title        string `json:"extract_title"`
}

// PostExtractFromChat is mounted at POST /c/{slug}/chat/extract by the
// router (the URL lives under /chat for discoverability, but the
// handler is in projects because it writes to project_attachments /
// project_issues). Mod / admin only.
//
//	mode=docs  → insert into project_attachments referring to the
//	             same uploads row the chat attachment points at.
//	mode=issue → create a project_issues row with title prefilled from
//	             filename (or signal) + a single project_issue_attachments
//	             row referring to the upload. Server redirects the
//	             caller to the new issue.
//
// Either way, chat_attachment_extracts records the link so the chat
// bubble can render the "↗ in X" badge. Original chat row is
// unchanged.
func (h *Handler) PostExtractFromChat(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	if !id.Membership.Role.AtLeast(auth.RoleMod) {
		http.Error(w, "moderator role required", http.StatusForbidden)
		return
	}
	if h.ChatRepo == nil {
		http.Error(w, "chat repo unavailable", http.StatusServiceUnavailable)
		return
	}
	c, ok := community.FromContext(r.Context())
	if !ok {
		http.Error(w, "no community", http.StatusInternalServerError)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	var in extractFromChatSignals
	if err := datastar.ReadSignals(r, &in); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	in.AttachmentID = strings.TrimSpace(in.AttachmentID)
	in.ProjectID = strings.TrimSpace(in.ProjectID)
	if in.Mode == "" {
		in.Mode = "docs"
	}
	if in.AttachmentID == "" || in.ProjectID == "" {
		http.Error(w, "missing attachment_id or project_id", http.StatusBadRequest)
		return
	}

	att, err := h.ChatRepo.AttachmentByID(r.Context(), in.AttachmentID)
	if err != nil {
		http.Error(w, "attachment not found", http.StatusNotFound)
		return
	}
	// Project must exist in the same community as the chat attachment.
	p, err := h.Repo.ByID(r.Context(), in.ProjectID)
	if err != nil || p.CommunityID != c.ID {
		http.Error(w, "project not found in this community", http.StatusNotFound)
		return
	}

	sse := render.NewSSE(w, r)
	now := time.Now().UTC()
	ex := chat.Extract{
		ID:               uuid.NewString(),
		ChatAttachmentID: att.ID,
		ProjectID:        p.ID,
		Mode:             in.Mode,
		CreatedAt:        now,
	}

	switch in.Mode {
	case "docs":
		paID := uuid.NewString()
		pa := Attachment{
			ID:         paID,
			ProjectID:  p.ID,
			UploadID:   att.UploadID,
			Filename:   att.Filename,
			MIME:       att.MIME,
			SizeBytes:  att.Size,
			UploaderID: id.User.ID,
			Category:   "chat-extract",
			CreatedAt:  now,
		}
		if err := h.Repo.InsertAttachment(r.Context(), pa); err != nil {
			h.Log.Error("extract → project attachment", "err", err)
			http.Error(w, "save failed", http.StatusInternalServerError)
			return
		}
		ex.ProjectAttachmentID = paID
	case "issue":
		title := strings.TrimSpace(in.Title)
		if title == "" {
			title = strings.TrimSuffix(att.Filename, lastDot(att.Filename))
		}
		if title == "" {
			title = "Extracted from chat"
		}
		callerID := Identity{UserID: id.User.ID}
		issue, err := h.Svc.CreateIssue(r.Context(), p.ID, title, "", callerID)
		if err != nil {
			h.Log.Error("extract → issue", "err", err)
			http.Error(w, "create issue failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		ia := IssueAttachment{
			ID:             uuid.NewString(),
			IssueID:        issue.ID,
			UploadID:       att.UploadID,
			UploaderUserID: id.User.ID,
			UploaderName:   id.Membership.DisplayName,
			CreatedAt:      now,
		}
		if err := h.Repo.InsertIssueAttachment(r.Context(), ia); err != nil {
			h.Log.Error("extract → issue attachment", "err", err)
			http.Error(w, "attach to issue failed", http.StatusInternalServerError)
			return
		}
		ex.IssueID = issue.ID
	default:
		http.Error(w, "unknown mode", http.StatusBadRequest)
		return
	}

	if err := h.ChatRepo.InsertExtract(r.Context(), ex); err != nil {
		h.Log.Warn("extract record", "err", err)
		// Non-fatal — the doc/issue is filed; only the badge state
		// is missing. The next page refresh will still show the
		// linked row in the target project; only the chat bubble
		// loses its "↗" badge.
	}

	// Wake every chat tab so the bubble re-renders with the badge.
	// (Cross-process NATS fan-out is handled by chat.Handler's own
	// broadcast path; this in-process Bus.Broadcast is enough for the
	// extracting tab's siblings.)
	if h.ChatBus != nil {
		h.ChatBus.Broadcast()
	}

	// Close the modal + clear signals. For issue mode, redirect the
	// extractor to the new issue page; for docs mode, stay on chat.
	_ = sse.PatchSignals([]byte(`{"_extract_open":false,"extract_attachment_id":"","extract_attachment_name":"","extract_project_id":"","extract_title":""}`))
	if in.Mode == "issue" && ex.IssueID != "" {
		_ = sse.Redirect("/c/" + c.Slug + "/projects/" + p.ID + "/issues/" + ex.IssueID)
	}
}

// lastDot returns the trailing dot-extension of a filename, or "".
func lastDot(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '.' {
			return name[i:]
		}
		if name[i] == '/' {
			return ""
		}
	}
	return ""
}
