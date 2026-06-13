package community

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

type Community struct {
	ID        string
	Slug      string
	Name      string
	CreatedAt time.Time
}

type Repo struct{ DB *sql.DB }

func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

func (r *Repo) BySlug(ctx context.Context, slug string) (Community, error) {
	var c Community
	var created int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, slug, name, created_at FROM communities WHERE slug = ?`, slug).
		Scan(&c.ID, &c.Slug, &c.Name, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Community{}, sql.ErrNoRows
	}
	if err != nil {
		return Community{}, err
	}
	c.CreatedAt = time.Unix(created, 0)
	return c, nil
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
