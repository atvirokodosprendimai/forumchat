// Package search fuses a community's two search indexes — the synchronous FTS5
// index (search_fts, migration 00038) and the asynchronous semantic vector index
// (internal/rag) — into one ranked result list via Reciprocal Rank Fusion, then
// resolves every hit to a deep link into the UI.
//
// It stays decoupled the way web/templ does (AGENTS.md §4.13): the semantic side
// arrives as a closure wired in main.go, so search imports neither rag nor agent.
// The full-text side is queried directly because search_fts always exists (it is
// kept live by SQL triggers, independent of any feature flag), whereas the
// semantic index is gated by RAG_ENABLED and its closure is nil when off.
//
// Link resolution reads parent ids straight from the DB because both indexes
// store only a row's own id (ref_id), not the project / thread it lives under —
// the same reason the global issues page joins back to projects for its hrefs.
package search

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"
)

// Content kinds, mirroring internal/rag's constants (duplicated so search stays a
// leaf — it never imports rag). FTS emits chat/thread/post; semantic adds the
// rest.
const (
	kindChat            = "chat"
	kindThread          = "thread"
	kindPost            = "post"
	kindIssue           = "issue"
	kindIssueComment    = "issue_comment"
	kindDiscussion      = "discussion"
	kindDiscussionReply = "discussion_reply"
	kindProject         = "project"
	kindAI              = "ai"
	kindPaste           = "paste"
	kindNote            = "note"
)

// rrfK is the Reciprocal Rank Fusion constant. 60 is the value from the original
// RRF paper (Cormack, Clarke & Büttcher 2009) and the de-facto industry default:
// big enough that the rank-1↔rank-2 gap doesn't dominate, small enough that deep
// ranks still contribute. RRF deliberately ignores each engine's raw score (BM25
// vs. cosine aren't comparable) and fuses on rank alone.
const rrfK = 60.0

// Result-list bounds and the per-index over-fetch before fusion.
const (
	DefaultLimit = 20
	MaxLimit     = 20
	perIndex     = 30
)

// Hit is one result from a single index, already ranked best-first. Both sides
// return these; the package never sees agent.SearchHit or rag.Hit.
type Hit struct {
	Kind      string
	RefID     string
	Title     string
	Snippet   string
	CreatedAt int64
}

// SearchFunc runs one index for a community, hits best-first. A nil SearchFunc is
// an empty index (the semantic side when RAG is disabled).
type SearchFunc func(ctx context.Context, communityID, query string, limit int) ([]Hit, error)

// Result is one fused, link-resolved row handed to the template.
type Result struct {
	Kind       string
	RefID      string
	Title      string
	Snippet    string // FTS «»-highlighted when the FTS index matched, else plain
	URL        string // deep link; "" if the row vanished between index and resolve
	CreatedAt  int64
	Score      float64 // fused RRF score, higher is better
	InFulltext bool
	InSemantic bool
}

// Service fuses the indexes and resolves links. Semantic is nil when RAG is off,
// in which case Search degrades to a plain full-text ranking.
type Service struct {
	DB       *sql.DB
	Semantic SearchFunc
}

// Search runs both indexes concurrently, fuses with RRF, caps to limit, and
// resolves one deep link per result. slug is the community's URL slug (links are
// built from it). A blank query returns nil.
// Search runs the indexes concurrently, fuses with RRF, caps to limit, and
// resolves one deep link per result. viewerID, when non-empty, also full-text
// searches that viewer's OWN private notes (note_private_fts) — the only path
// that surfaces a private note in search, and only to its author. slug is the
// community's URL slug. A blank query returns nil.
func (s *Service) Search(ctx context.Context, communityID, viewerID, slug, query string, limit int) ([]Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if limit <= 0 || limit > MaxLimit {
		limit = DefaultLimit
	}

	var (
		wg                         sync.WaitGroup
		ftsHits, vecHits, privHits []Hit
		ftsErr, vecErr, privErr    error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		ftsHits, ftsErr = s.fulltext(ctx, communityID, query, perIndex)
	}()
	go func() {
		defer wg.Done()
		if s.Semantic != nil {
			vecHits, vecErr = s.Semantic(ctx, communityID, query, perIndex)
		}
	}()
	if viewerID != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			privHits, privErr = s.privateNotes(ctx, communityID, viewerID, query, perIndex)
		}()
	}
	wg.Wait()
	if ftsErr != nil {
		return nil, fmt.Errorf("fulltext search: %w", ftsErr)
	}
	if vecErr != nil {
		return nil, fmt.Errorf("semantic search: %w", vecErr)
	}
	if privErr != nil {
		return nil, fmt.Errorf("private notes search: %w", privErr)
	}
	// Private-note hits are full-text results for the author; fold them into the
	// FTS list (no key collision — a note is public OR private, never both).
	ftsHits = append(ftsHits, privHits...)

	fused := fuse(query, ftsHits, vecHits)
	if len(fused) > limit {
		fused = fused[:limit]
	}
	s.resolveLinks(ctx, slug, fused)
	return fused, nil
}

// fuse merges two ranked lists with Reciprocal Rank Fusion. Each list contributes
// 1/(rrfK+rank) (rank 0-based) to a hit keyed by kind:ref_id; a hit in both lists
// sums both contributions. The fused score is then weighted by content kind and
// boosted when the query matches the result's title, so structured content and
// exact title hits rank above ephemeral chat. Sorted best-first, ties by recency.
func fuse(query string, fts, vec []Hit) []Result {
	byKey := map[string]*Result{}
	order := make([]*Result, 0, len(fts)+len(vec))
	apply := func(hits []Hit, fromFTS bool) {
		for rank, h := range hits {
			key := h.Kind + ":" + h.RefID
			r := byKey[key]
			if r == nil {
				r = &Result{Kind: h.Kind, RefID: h.RefID, Title: h.Title, Snippet: h.Snippet, CreatedAt: h.CreatedAt}
				byKey[key] = r
				order = append(order, r)
			}
			r.Score += 1.0 / (rrfK + float64(rank))
			if fromFTS {
				r.InFulltext = true
				if h.Snippet != "" { // FTS snippet carries «» highlighting — prefer it
					r.Snippet = h.Snippet
				}
			} else {
				r.InSemantic = true
				if r.Title == "" {
					r.Title = h.Title
				}
				if r.Snippet == "" {
					r.Snippet = h.Snippet
				}
			}
		}
	}
	apply(fts, true)
	apply(vec, false)

	out := make([]Result, 0, len(order))
	for _, r := range order {
		// Weight the rank-fusion score by content kind (durable, structured
		// content outranks ephemeral chat) and add a title-match bonus so an
		// exact title hit floats to the top regardless of which index found it
		// or how fuzzy the vector rank was.
		r.Score = r.Score*kindWeight(r.Kind) + titleBonus(query, r.Title)
		out = append(out, *r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].CreatedAt > out[j].CreatedAt
	})
	return out
}

// kindWeight ranks content types. Projects, issues and threads are durable,
// authored, titled artifacts; chat is ephemeral. Tuned so a structured hit
// outranks a chat hit at a comparable RRF rank, matching the requested order:
// project > issue > thread > discussion > agent thread > chat.
func kindWeight(kind string) float64 {
	switch kind {
	case kindProject:
		return 1.6
	case kindIssue, kindIssueComment:
		return 1.45
	case kindThread, kindPost:
		return 1.3
	case kindPaste:
		// A paste is a durable, authored, titled code/text snippet — rank it
		// alongside threads, above ephemeral chat.
		return 1.3
	case kindDiscussion, kindDiscussionReply:
		return 1.2
	case kindAI:
		return 1.15
	default: // chat
		return 1.0
	}
}

// titleBonus rewards a result whose title matches the query. Exact (normalized)
// match dominates any RRF score so a 100%-title hit lands on top; prefix and
// substring matches get smaller, still-decisive boosts. Untitled kinds (chat,
// replies, comments) have an empty title and so never receive a bonus — the
// boost is structurally limited to titled content (projects/issues/threads/
// discussions).
func titleBonus(query, title string) float64 {
	q := normalizeText(query)
	t := normalizeText(title)
	if q == "" || t == "" {
		return 0
	}
	switch {
	case t == q:
		return 0.5
	case strings.HasPrefix(t, q):
		return 0.05
	case strings.Contains(t, q):
		return 0.02
	}
	return 0
}

// normalizeText lower-cases and collapses internal whitespace for matching.
func normalizeText(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// fulltext queries search_fts directly (BM25-ranked). Tokens are quoted so
// arbitrary user input never trips FTS5 query syntax. Mirrors
// agent.Repo.SearchContent, duplicated to keep search independent of the
// AI-feature-flagged agent package.
func (s *Service) fulltext(ctx context.Context, communityID, query string, limit int) ([]Hit, error) {
	match := ftsQuery(query)
	if match == "" {
		return nil, nil
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT kind, ref_id, title, snippet(search_fts, 1, '«', '»', '…', 12), created_at
		FROM search_fts
		WHERE community_id = ? AND search_fts MATCH ?
		ORDER BY bm25(search_fts) LIMIT ?`,
		communityID, match, limit)
	if err != nil {
		return nil, fmt.Errorf("fts query: %w", err)
	}
	defer rows.Close()
	var out []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.Kind, &h.RefID, &h.Title, &h.Snippet, &h.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan hit: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// privateNotes full-text searches the viewer's OWN private notes
// (note_private_fts, migration 00065), scoped to (community, author). This is
// the ONLY path that surfaces a private note in search, and only to its author —
// the rows never enter the community-wide search_fts. Returns kind='note' hits
// so they link and render exactly like a public note result.
func (s *Service) privateNotes(ctx context.Context, communityID, authorID, query string, limit int) ([]Hit, error) {
	match := ftsQuery(query)
	if match == "" {
		return nil, nil
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT 'note', ref_id, title, snippet(note_private_fts, 1, '«', '»', '…', 12), created_at
		FROM note_private_fts
		WHERE community_id = ? AND author_id = ? AND note_private_fts MATCH ?
		ORDER BY bm25(note_private_fts) LIMIT ?`,
		communityID, authorID, match, limit)
	if err != nil {
		return nil, fmt.Errorf("private notes fts: %w", err)
	}
	defer rows.Close()
	var out []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.Kind, &h.RefID, &h.Title, &h.Snippet, &h.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan private hit: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ftsQuery turns free text into a safe FTS5 MATCH expression: each whitespace
// token becomes a double-quoted term (implicit AND), so FTS5 operators in the
// input are treated literally instead of erroring.
func ftsQuery(q string) string {
	fields := strings.Fields(q)
	if len(fields) == 0 {
		return ""
	}
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, `"`+strings.ReplaceAll(f, `"`, `""`)+`"`)
	}
	return strings.Join(parts, " ")
}

// resolveLinks fills Result.URL for every result. Direct-id kinds are built
// inline; kinds whose link needs a parent id (post→thread, issue→project, …) are
// resolved in one batched query per kind. A row that vanished between indexing
// and now keeps URL="" and is dropped by the handler.
func (s *Service) resolveLinks(ctx context.Context, slug string, rs []Result) {
	base := "/c/" + slug
	byKind := map[string][]string{}
	for i := range rs {
		switch rs[i].Kind {
		case kindThread:
			rs[i].URL = base + "/forum/" + rs[i].RefID
		case kindProject:
			rs[i].URL = base + "/projects/" + rs[i].RefID
		case kindPaste:
			rs[i].URL = base + "/pastes/" + rs[i].RefID
		case kindNote:
			rs[i].URL = base + "/notes/" + rs[i].RefID
		case kindChat:
			// No per-message permalink exists; the history day-log anchors each
			// chat row as <li id="msg-{id}">, so jump there by date.
			rs[i].URL = base + "/history?d=" + chatDay(rs[i].CreatedAt) + "#msg-" + rs[i].RefID
		default:
			byKind[rs[i].Kind] = append(byKind[rs[i].Kind], rs[i].RefID)
		}
	}
	links := map[string]string{} // "kind:ref_id" -> url
	for kind, ids := range byKind {
		maps.Copy(links, s.resolveKind(ctx, base, kind, ids))
	}
	for i := range rs {
		if rs[i].URL == "" {
			rs[i].URL = links[rs[i].Kind+":"+rs[i].RefID]
		}
	}
	s.dropDeletedChat(ctx, rs)
}

// dropDeletedChat blanks the URL of any chat result whose live row is
// soft-deleted (or gone), so the handler drops it. The FTS index clears chat
// rows synchronously on soft-delete, but the semantic (RAG) vector store lags by
// one worker tick and still holds the deleted body's snippet — without this
// re-check a removed chat message could surface in semantic search during that
// window. Deleted chat content is hidden from everyone, search included.
func (s *Service) dropDeletedChat(ctx context.Context, rs []Result) {
	ids := make([]string, 0)
	for i := range rs {
		if rs[i].Kind == kindChat && rs[i].URL != "" {
			ids = append(ids, rs[i].RefID)
		}
	}
	if len(ids) == 0 {
		return
	}
	live, err := s.liveChatIDs(ctx, ids)
	for i := range rs {
		if rs[i].Kind != kindChat || rs[i].URL == "" {
			continue
		}
		// Fail closed: if liveness couldn't be verified, drop the chat hit
		// rather than risk surfacing a removed message.
		if err != nil || !live[rs[i].RefID] {
			rs[i].URL = "" // dropped by Views
		}
	}
}

// liveChatIDs returns the subset of ids whose chat_messages row exists and is
// not soft-deleted.
func (s *Service) liveChatIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	ph, args := placeholders(ids)
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id FROM chat_messages WHERE id IN (`+ph+`) AND deleted_at IS NULL`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	live := make(map[string]bool, len(ids))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		live[id] = true
	}
	return live, rows.Err()
}

// resolveKind batch-resolves one parent-needing kind to a map of
// "kind:ref_id" → deep-link URL. Each branch's JOINs mirror the rag loaders.
func (s *Service) resolveKind(ctx context.Context, base, kind string, ids []string) map[string]string {
	if len(ids) == 0 {
		return nil
	}
	ph, args := placeholders(ids)
	var (
		query string
		build func(scan func(...any) error) (key, url string, err error)
	)
	switch kind {
	case kindPost:
		query = `SELECT id, thread_id FROM posts WHERE id IN (` + ph + `)`
		build = func(scan func(...any) error) (string, string, error) {
			var id, thread string
			if err := scan(&id, &thread); err != nil {
				return "", "", err
			}
			return kind + ":" + id, base + "/forum/" + thread + "#post-" + id, nil
		}
	case kindAI:
		query = `SELECT id, thread_id FROM ai_messages WHERE id IN (` + ph + `)`
		build = func(scan func(...any) error) (string, string, error) {
			var id, thread string
			if err := scan(&id, &thread); err != nil {
				return "", "", err
			}
			return kind + ":" + id, base + "/agent/" + thread, nil
		}
	case kindIssue:
		query = `SELECT id, project_id FROM project_issues WHERE id IN (` + ph + `)`
		build = func(scan func(...any) error) (string, string, error) {
			var id, project string
			if err := scan(&id, &project); err != nil {
				return "", "", err
			}
			return kind + ":" + id, base + "/projects/" + project + "/issues/" + id, nil
		}
	case kindIssueComment:
		query = `SELECT c.id, i.project_id, i.id
			FROM project_issue_comments c JOIN project_issues i ON i.id = c.issue_id
			WHERE c.id IN (` + ph + `)`
		build = func(scan func(...any) error) (string, string, error) {
			var id, project, issue string
			if err := scan(&id, &project, &issue); err != nil {
				return "", "", err
			}
			return kind + ":" + id, base + "/projects/" + project + "/issues/" + issue, nil
		}
	case kindDiscussion:
		query = `SELECT id, project_id FROM project_discussion_threads WHERE id IN (` + ph + `)`
		build = func(scan func(...any) error) (string, string, error) {
			var id, project string
			if err := scan(&id, &project); err != nil {
				return "", "", err
			}
			return kind + ":" + id, base + "/projects/" + project + "/discussions/" + id, nil
		}
	case kindDiscussionReply:
		query = `SELECT rp.id, d.project_id, d.id
			FROM project_discussion_replies rp JOIN project_discussion_threads d ON d.id = rp.thread_id
			WHERE rp.id IN (` + ph + `)`
		build = func(scan func(...any) error) (string, string, error) {
			var id, project, thread string
			if err := scan(&id, &project, &thread); err != nil {
				return "", "", err
			}
			return kind + ":" + id, base + "/projects/" + project + "/discussions/" + thread, nil
		}
	default:
		return nil
	}

	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make(map[string]string, len(ids))
	for rows.Next() {
		key, u, err := build(rows.Scan)
		if err != nil {
			continue
		}
		out[key] = u
	}
	return out
}

// placeholders builds "?,?,…" plus the matching args slice for an IN clause.
func placeholders(ids []string) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

// chatDay formats a unix time as the local YYYY-MM-DD the history page expects.
func chatDay(unix int64) string {
	return time.Unix(unix, 0).In(time.Local).Format("2006-01-02")
}
