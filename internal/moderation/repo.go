package moderation

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"
)

// Flag is one recorded policy hit. It deliberately carries NO message body —
// only the reference and the category codes — so the audit can never expose
// tenant content.
type Flag struct {
	CommunityID string
	MessageID   string
	ChannelID   string
	AuthorID    string
	Categories  string // CSV of Llama Guard codes, e.g. "S3,S12"
	Model       string
}

// Repo persists moderation flags. Stateless; construct with the shared *sql.DB.
type Repo struct{ DB *sql.DB }

// NewRepo returns a Repo over db.
func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

// Insert records one flag. channel_id and author_id are stored as NULL when
// empty so the FK/soft-stamp semantics in the migration hold.
func (r *Repo) Insert(ctx context.Context, f Flag) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO moderation_flags
		  (id, community_id, message_id, channel_id, author_id, categories, model, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), f.CommunityID, f.MessageID,
		nullStr(f.ChannelID), nullStr(f.AuthorID), f.Categories, f.Model, time.Now().Unix())
	return err
}

// FlagRow is one recorded flag joined with its community + author for the
// super-admin audit list. It carries NO message body (none is stored) — only the
// reference, the categories, and who/where, so the operator can act without
// reading content.
type FlagRow struct {
	CreatedAt     int64
	MessageID     string
	ChannelID     string
	Categories    string // raw CSV as stored
	Model         string
	CommunityName string
	CommunitySlug string
	AuthorEmail   string // "" when the author was erased (author_id SET NULL)
}

// Recent returns the newest flags across all communities, for the super-admin
// moderation audit card. limit caps the rows (<=0 falls back to 50).
func (r *Repo) Recent(ctx context.Context, limit int) ([]FlagRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.DB.QueryContext(ctx, `
		SELECT mf.created_at, mf.message_id, COALESCE(mf.channel_id,''), mf.categories, mf.model,
		       COALESCE(c.name,''), COALESCE(c.slug,''), COALESCE(u.email,'')
		FROM moderation_flags mf
		LEFT JOIN communities c ON c.id = mf.community_id
		LEFT JOIN users u ON u.id = mf.author_id
		ORDER BY mf.created_at DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FlagRow
	for rows.Next() {
		var f FlagRow
		if err := rows.Scan(&f.CreatedAt, &f.MessageID, &f.ChannelID, &f.Categories, &f.Model,
			&f.CommunityName, &f.CommunitySlug, &f.AuthorEmail); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// nullStr maps "" to a SQL NULL so optional columns stay null, not empty string.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
