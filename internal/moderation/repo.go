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

// nullStr maps "" to a SQL NULL so optional columns stay null, not empty string.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
