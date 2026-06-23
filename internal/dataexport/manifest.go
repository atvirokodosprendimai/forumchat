package dataexport

import "strings"

// tableSpec describes one exported table: where its JSON lands in the ZIP
// (folder/file.json), the table to read, the WHERE clause scoping it to ONE
// community, and any extra columns to drop on top of the global secret rule.
//
// table and where are internal constants — never user input — so string
// interpolation into SQL is safe here (same property as uploads.deleteWhere).
// Every "?" in where is bound to the community id, so multi-level subqueries
// just repeat the placeholder.
type tableSpec struct {
	folder string
	file   string
	table  string
	where  string
	skip   []string // extra non-secret columns to omit (secrets are dropped globally)
}

// jsonPath is the ZIP entry path for this spec, e.g. "chat/messages.json".
func (t tableSpec) jsonPath() string { return t.folder + "/" + t.file + ".json" }

// scopedToProject / scopedToThread are the two recurring subquery shapes.
const (
	inCommunityProjects = `project_id IN (SELECT id FROM projects WHERE community_id = ?)`
	inCommunityThreads  = `thread_id IN (SELECT id FROM threads WHERE community_id = ?)`
)

// manifest is the full set of business functions exported. Adding a table is one
// line. Tables deliberately omitted: sessions, verification/signup tokens, push
// subscriptions, read-state, OAuth identities, debug logs, embed_outbox (RAG
// vectors — platform property), ai_configs/ai_mcp_servers (secrets/infra),
// private DMs (cross-party), and migration-artifact tables.
var manifest = []tableSpec{
	// community
	{"community", "community", "communities", `id = ?`, nil},
	{"community", "settings", "community_settings", `community_id = ?`, nil},

	// members
	{"members", "memberships", "memberships", `community_id = ?`, nil},
	{"members", "members", "users", `id IN (SELECT user_id FROM memberships WHERE community_id = ?)`, nil},
	{"members", "invites", "invite_codes", `community_id = ?`, nil},
	{"members", "bookmarks", "bookmarks", `community_id = ?`, nil},
	{"members", "todos", "todos", `community_id = ?`, nil},
	{"members", "blocks", "user_blocks", `community_id = ?`, nil},
	{"members", "reports", "user_reports", `community_id = ?`, nil},

	// chat
	{"chat", "channels", "chat_channels", `community_id = ?`, nil},
	{"chat", "messages", "chat_messages", `community_id = ?`, nil},
	{"chat", "message_attachments", "chat_message_attachments", `chat_message_id IN (SELECT id FROM chat_messages WHERE community_id = ?)`, nil},
	{"chat", "pastes", "pastes", `community_id = ?`, nil},

	// forum
	{"forum", "threads", "threads", `community_id = ?`, nil},
	{"forum", "posts", "posts", inCommunityThreads, nil},

	// agents — conversations only; the agent's system prompt + model config are
	// platform property (the customer's own instruction), so they're dropped.
	{"agents", "agents", "ai_agents", `community_id = ?`, []string{"system_prompt", "base_url", "provider", "model", "vision"}},
	{"agents", "threads", "ai_threads", `community_id = ?`, nil},
	{"agents", "messages", "ai_messages", `thread_id IN (SELECT id FROM ai_threads WHERE community_id = ?)`, nil},

	// projects
	{"projects", "projects", "projects", `community_id = ?`, nil},
	{"projects", "comments", "project_comments", inCommunityProjects, nil},
	{"projects", "todos", "project_todos", inCommunityProjects, nil},
	{"projects", "attachments", "project_attachments", inCommunityProjects, nil},
	{"projects", "guest_invites", "project_guest_invites", inCommunityProjects, nil},
	{"projects", "issues", "project_issues", inCommunityProjects, nil},
	{"projects", "issue_comments", "project_issue_comments", `issue_id IN (SELECT id FROM project_issues WHERE ` + inCommunityProjects + `)`, nil},
	{"projects", "issue_attachments", "project_issue_attachments", `issue_id IN (SELECT id FROM project_issues WHERE ` + inCommunityProjects + `)`, nil},
	{"projects", "discussion_threads", "project_discussion_threads", inCommunityProjects, nil},
	{"projects", "discussion_replies", "project_discussion_replies", `thread_id IN (SELECT id FROM project_discussion_threads WHERE ` + inCommunityProjects + `)`, nil},
	{"projects", "time_entries", "time_entries", `community_id = ?`, nil},
	{"projects", "time_budgets", "time_budgets", `community_id = ?`, nil},

	// lobbies
	{"lobbies", "lobbies", "lobbies", `community_id = ?`, nil},
	{"lobbies", "messages", "lobby_messages", `lobby_id IN (SELECT id FROM lobbies WHERE community_id = ?)`, nil},

	// rooms
	{"rooms", "rooms", "rooms", `community_id = ?`, nil},
	{"rooms", "room_chat", "room_chat", `community_id = ?`, nil},

	// webhooks — the token + signing secret are dropped by the secret rule.
	{"webhooks", "webhooks", "webhooks", `community_id = ?`, nil},

	// mailbox
	{"mailbox", "filters", "community_mail_filter", `community_id = ?`, nil},
	{"mailbox", "email_ingest", "email_ingest", `community_id = ?`, nil},
}

// redactColumn reports whether a column is a secret that must never appear in an
// export, by name rule. Caught: password_hash, bare token/secret, and anything
// ending _enc / _key / _token / _secret or containing api_key / secret /
// password. This is intentionally broad — a new secret column is redacted by
// default rather than leaking until someone remembers to add it here.
func redactColumn(col string) bool {
	switch col {
	case "password_hash", "token", "secret":
		return true
	}
	for _, suf := range []string{"_enc", "_key", "_token", "_secret"} {
		if strings.HasSuffix(col, suf) {
			return true
		}
	}
	return strings.Contains(col, "api_key") || strings.Contains(col, "secret") || strings.Contains(col, "password")
}
