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
