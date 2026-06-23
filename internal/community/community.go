package community

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/secretbox"
)

type Community struct {
	ID        string
	Slug      string
	Name      string
	IsPublic  bool
	CreatedAt time.Time

	// AgentRatePerUserMin / AgentRatePerCommunityMin cap AI-agent prompts per
	// minute (0 = unlimited). Per-user is community-wide; per-community is all
	// members combined. Only populated by BySlug / ByID.
	AgentRatePerUserMin      int
	AgentRatePerCommunityMin int
}

type Repo struct {
	DB *sql.DB
	// Secrets seals/opens per-community secret settings (Qdrant/S3 keys) at
	// rest. Optional — nil falls back to a passthrough box (dev/tests). Wired
	// in main.go from config.SecretsKey.
	Secrets *secretbox.Box
}

func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

func (r *Repo) BySlug(ctx context.Context, slug string) (Community, error) {
	var c Community
	var created int64
	var isPublic int
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, slug, name, COALESCE(is_public,0), created_at,
		       COALESCE(agent_rate_per_user_min,0), COALESCE(agent_rate_per_community_min,0)
		FROM communities WHERE slug = ?`, slug).
		Scan(&c.ID, &c.Slug, &c.Name, &isPublic, &created, &c.AgentRatePerUserMin, &c.AgentRatePerCommunityMin)
	if errors.Is(err, sql.ErrNoRows) {
		return Community{}, sql.ErrNoRows
	}
	if err != nil {
		return Community{}, err
	}
	c.IsPublic = isPublic != 0
	c.CreatedAt = time.Unix(created, 0)
	return c, nil
}

// ByID is the same lookup as BySlug but keyed by primary id. Used by
// callers that have a community id stashed elsewhere (e.g. a room row)
// and need to resolve back to the slug for URL building.
func (r *Repo) ByID(ctx context.Context, id string) (Community, error) {
	var c Community
	var created int64
	var isPublic int
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, slug, name, COALESCE(is_public,0), created_at,
		       COALESCE(agent_rate_per_user_min,0), COALESCE(agent_rate_per_community_min,0)
		FROM communities WHERE id = ?`, id).
		Scan(&c.ID, &c.Slug, &c.Name, &isPublic, &created, &c.AgentRatePerUserMin, &c.AgentRatePerCommunityMin)
	if errors.Is(err, sql.ErrNoRows) {
		return Community{}, sql.ErrNoRows
	}
	if err != nil {
		return Community{}, err
	}
	c.IsPublic = isPublic != 0
	c.CreatedAt = time.Unix(created, 0)
	return c, nil
}

// SetAgentRateLimits updates a community's AI-agent prompt rate limits
// (requests/minute, 0 = unlimited). Negative inputs are clamped to 0.
func (r *Repo) SetAgentRateLimits(ctx context.Context, id string, perUserMin, perCommunityMin int) error {
	if perUserMin < 0 {
		perUserMin = 0
	}
	if perCommunityMin < 0 {
		perCommunityMin = 0
	}
	_, err := r.DB.ExecContext(ctx, `
		UPDATE communities SET agent_rate_per_user_min = ?, agent_rate_per_community_min = ? WHERE id = ?`,
		perUserMin, perCommunityMin, id)
	return err
}

// SetPublic flips the discoverability flag on a community.
func (r *Repo) SetPublic(ctx context.Context, id string, public bool) error {
	v := 0
	if public {
		v = 1
	}
	_, err := r.DB.ExecContext(ctx, `UPDATE communities SET is_public = ? WHERE id = ?`, v, id)
	return err
}

// PublicListing is one row of the explore page.
type PublicListing struct {
	Community
	MemberCount int
	IsMember    bool
	IsPending   bool
}

// ListPublic returns public communities annotated with the viewer's
// membership state so the explore page can pick the right CTA.
// viewerID may be empty for anonymous browsing (everything reads as "not a member").
func (r *Repo) ListPublic(ctx context.Context, viewerID string) ([]PublicListing, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT c.id, c.slug, c.name, c.created_at,
		       (SELECT COUNT(*) FROM memberships mb2
		          WHERE mb2.community_id = c.id AND mb2.approved_at IS NOT NULL) AS member_count,
		       mb.approved_at IS NOT NULL AS is_member,
		       mb.id IS NOT NULL AND mb.approved_at IS NULL AS is_pending
		FROM communities c
		LEFT JOIN memberships mb ON mb.community_id = c.id AND mb.user_id = ?
		WHERE c.is_public = 1
		ORDER BY member_count DESC, c.name`, viewerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PublicListing
	for rows.Next() {
		var row PublicListing
		var created int64
		var isMember, isPending int
		if err := rows.Scan(&row.Community.ID, &row.Community.Slug, &row.Community.Name, &created,
			&row.MemberCount, &isMember, &isPending); err != nil {
			return nil, err
		}
		row.Community.IsPublic = true
		row.Community.CreatedAt = time.Unix(created, 0)
		row.IsMember = isMember != 0
		row.IsPending = isPending != 0
		out = append(out, row)
	}
	return out, rows.Err()
}

// Create inserts a new community. Returns ErrSlugTaken when the slug is
// already in use.
func (r *Repo) Create(ctx context.Context, slug, name string) (Community, error) {
	if existing, err := r.BySlug(ctx, slug); err == nil {
		_ = existing
		return Community{}, ErrSlugTaken
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Community{}, err
	}
	c := Community{
		ID:        uuid.NewString(),
		Slug:      slug,
		Name:      name,
		CreatedAt: time.Now(),
	}
	if _, err := r.DB.ExecContext(ctx, `
		INSERT INTO communities (id, slug, name, created_at) VALUES (?, ?, ?, ?)`,
		c.ID, c.Slug, c.Name, c.CreatedAt.Unix()); err != nil {
		return Community{}, err
	}
	return c, nil
}

var ErrSlugTaken = errors.New("community: slug already taken")

// CommunityStat is one row of the /superadmin community roster: a community
// plus a count of the heaviest content a delete would destroy. The counts
// power the honest destructive-delete confirmation (see the super-admin
// handler) — they are an indication of blast radius, NOT an exhaustive
// inventory of every cascading table.
type CommunityStat struct {
	Community
	MemberCount  int
	MessageCount int
	ThreadCount  int
}

// ListAll returns every community with its approved-member, chat-message and
// thread counts, newest first. Drives the platform super-admin dashboard.
func (r *Repo) ListAll(ctx context.Context) ([]CommunityStat, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT c.id, c.slug, c.name, COALESCE(c.is_public,0), c.created_at,
		       (SELECT COUNT(*) FROM memberships mb
		          WHERE mb.community_id = c.id AND mb.approved_at IS NOT NULL) AS member_count,
		       (SELECT COUNT(*) FROM chat_messages cm WHERE cm.community_id = c.id) AS message_count,
		       (SELECT COUNT(*) FROM threads t WHERE t.community_id = c.id) AS thread_count
		FROM communities c
		ORDER BY c.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommunityStat
	for rows.Next() {
		var row CommunityStat
		var created int64
		var isPublic int
		if err := rows.Scan(&row.Community.ID, &row.Community.Slug, &row.Community.Name,
			&isPublic, &created, &row.MemberCount, &row.MessageCount, &row.ThreadCount); err != nil {
			return nil, err
		}
		row.Community.IsPublic = isPublic != 0
		row.Community.CreatedAt = time.Unix(created, 0)
		out = append(out, row)
	}
	return out, rows.Err()
}

// Delete removes a community row. ⚠️ This is DESTRUCTIVE: most community-owned
// tables (memberships, invites, chat_messages, threads, channels, rooms,
// projects, todos, bookmarks, mailbox rows, …) declare
// `REFERENCES communities(id) ON DELETE CASCADE`, so deleting the community
// cascades and erases that data too. It does NOT "fail safely" when content
// exists. The caller (super-admin handler) is responsible for confirmation
// (slug match), surfacing the blast radius, and auditing.
func (r *Repo) Delete(ctx context.Context, id string) error {
	_, err := r.DB.ExecContext(ctx, `DELETE FROM communities WHERE id = ?`, id)
	return err
}

// MembershipRow holds a single community + the viewer's role in it. Drives
// the dashboard listing.
type MembershipRow struct {
	Community  Community
	Role       string
	IsApproved bool
	IsBanned   bool
}

// ListForUser returns every community the user belongs to with their role
// and approval/ban state. Excludes rejected memberships.
func (r *Repo) ListForUser(ctx context.Context, userID string) ([]MembershipRow, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT c.id, c.slug, c.name, c.created_at,
		       mb.role,
		       COALESCE(mb.approved_at, 0),
		       COALESCE(mb.banned_until, 0)
		FROM communities c
		JOIN memberships mb ON mb.community_id = c.id
		WHERE mb.user_id = ?
		ORDER BY c.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MembershipRow
	now := time.Now().Unix()
	for rows.Next() {
		var row MembershipRow
		var created int64
		var approvedAt, bannedUntil int64
		if err := rows.Scan(&row.Community.ID, &row.Community.Slug, &row.Community.Name, &created,
			&row.Role, &approvedAt, &bannedUntil); err != nil {
			return nil, err
		}
		row.Community.CreatedAt = time.Unix(created, 0)
		row.IsApproved = approvedAt > 0
		row.IsBanned = bannedUntil > now
		out = append(out, row)
	}
	return out, rows.Err()
}

// ctx key for the resolved community.
type ctxKey int

const communityCtxKey ctxKey = 0

func WithContext(ctx context.Context, c Community) context.Context {
	return context.WithValue(ctx, communityCtxKey, c)
}

// FromContext returns the resolved community for this request and ok=true.
// Use inside handlers mounted under the /c/{slug} group.
func FromContext(ctx context.Context) (Community, bool) {
	c, ok := ctx.Value(communityCtxKey).(Community)
	return c, ok
}

// MustFromContext panics if no community is on the context. Use only in
// handlers guaranteed to run inside the /c/{slug} group.
func MustFromContext(ctx context.Context) Community {
	c, ok := FromContext(ctx)
	if !ok {
		panic("community: missing from context — handler mounted outside /c/{slug}?")
	}
	return c
}

// BootstrapOrFetch creates a community with the given slug+name if none exists,
// otherwise returns the existing community by slug.
func (r *Repo) BootstrapOrFetch(ctx context.Context, slug, name string) (Community, error) {
	existing, err := r.BySlug(ctx, slug)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Community{}, err
	}
	c := Community{
		ID:        uuid.NewString(),
		Slug:      slug,
		Name:      name,
		CreatedAt: time.Now(),
	}
	if _, err := r.DB.ExecContext(ctx, `
		INSERT INTO communities (id, slug, name, created_at) VALUES (?, ?, ?, ?)`,
		c.ID, c.Slug, c.Name, c.CreatedAt.Unix()); err != nil {
		return Community{}, err
	}
	return c, nil
}
