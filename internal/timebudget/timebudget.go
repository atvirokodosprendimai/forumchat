// Package timebudget implements the per-community time accounting feature:
// a single recurring monthly budget (in minutes) plus a log of manual time
// entries. The whole community is treated as one client — admins set the
// monthly budget, admins/moderators log time spent (optionally tagged to a
// project), and every approved member can see how much of the month's budget
// is used vs remaining so they can plan upcoming work.
//
// Budgets reset every calendar month: "used" is the sum of entries dated in
// the current month, "remaining" is budget − used (negative means over).
// Feature gating happens at route-mount time via config.TimeEnabled; the
// tables always exist so toggling the flag never needs a schema migration.
package timebudget

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound is returned when an entry to delete does not exist (or is not
// visible to the requester).
var ErrNotFound = errors.New("timebudget: not found")

// Budget is the per-community recurring monthly budget. MonthlyMinutes == 0
// means no budget has been set yet.
type Budget struct {
	CommunityID    string
	MonthlyMinutes int
	UpdatedBy      string
	UpdatedAt      time.Time
}

// Entry is one manual time-log row.
type Entry struct {
	ID          string
	CommunityID string
	ProjectID   string // "" when untagged
	Minutes     int
	Note        string
	OccurredOn  string // 'YYYY-MM-DD'
	CreatedBy   string
	CreatedAt   time.Time
}

// EntryRow is Entry plus the joined display fields used by the page.
type EntryRow struct {
	Entry
	ProjectTitle string // "" when untagged or project deleted
	CreatorName  string
}

type Repo struct{ DB *sql.DB }

func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

// GetBudget returns the community's budget row, or a zero-value Budget
// (MonthlyMinutes == 0) when none has been set.
func (r *Repo) GetBudget(ctx context.Context, communityID string) (Budget, error) {
	var b Budget
	var updated int64
	err := r.DB.QueryRowContext(ctx, `
		SELECT community_id, monthly_minutes, updated_by, updated_at
		FROM time_budgets WHERE community_id = ?`, communityID).
		Scan(&b.CommunityID, &b.MonthlyMinutes, &b.UpdatedBy, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Budget{CommunityID: communityID}, nil
	}
	if err != nil {
		return Budget{}, err
	}
	b.UpdatedAt = time.Unix(updated, 0)
	return b, nil
}

// SetBudget upserts the community's monthly budget (in minutes).
func (r *Repo) SetBudget(ctx context.Context, communityID string, minutes int, updatedBy string) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO time_budgets (community_id, monthly_minutes, updated_by, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(community_id) DO UPDATE SET
			monthly_minutes = excluded.monthly_minutes,
			updated_by      = excluded.updated_by,
			updated_at      = excluded.updated_at`,
		communityID, minutes, updatedBy, time.Now().Unix())
	return err
}

// AddEntry inserts a time entry and returns it. Caller fills CommunityID /
// ProjectID / Minutes / Note / OccurredOn / CreatedBy; the rest is filled here.
func (r *Repo) AddEntry(ctx context.Context, e Entry) (Entry, error) {
	now := time.Now()
	e.ID = uuid.NewString()
	e.CreatedAt = now
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO time_entries
		(id, community_id, project_id, minutes, note, occurred_on, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.CommunityID, nullableString(e.ProjectID), e.Minutes, e.Note,
		e.OccurredOn, e.CreatedBy, now.Unix())
	if err != nil {
		return Entry{}, err
	}
	return e, nil
}

// DeleteEntry removes an entry. When all is false the delete is scoped to the
// requester's own rows (so a member can only remove what they logged).
func (r *Repo) DeleteEntry(ctx context.Context, communityID, entryID, requesterID string, all bool) error {
	query := `DELETE FROM time_entries WHERE id = ? AND community_id = ?`
	args := []any{entryID, communityID}
	if !all {
		query += ` AND created_by = ?`
		args = append(args, requesterID)
	}
	res, err := r.DB.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UsedMinutes sums the minutes of entries dated in the given month
// (ym = 'YYYY-MM').
func (r *Repo) UsedMinutes(ctx context.Context, communityID, ym string) (int, error) {
	var total sql.NullInt64
	err := r.DB.QueryRowContext(ctx, `
		SELECT SUM(minutes) FROM time_entries
		WHERE community_id = ? AND substr(occurred_on, 1, 7) = ?`,
		communityID, ym).Scan(&total)
	if err != nil {
		return 0, err
	}
	return int(total.Int64), nil
}

// ListEntries returns the month's entries (newest first), with the project
// title and creator display name joined for display.
func (r *Repo) ListEntries(ctx context.Context, communityID, ym string) ([]EntryRow, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT e.id, e.community_id, COALESCE(e.project_id, ''), e.minutes, e.note,
		       e.occurred_on, e.created_by, e.created_at,
		       COALESCE(p.title, ''), COALESCE(m.effective_display_name, '')
		FROM time_entries e
		LEFT JOIN projects p ON p.id = e.project_id
		LEFT JOIN memberships m ON m.user_id = e.created_by AND m.community_id = e.community_id
		WHERE e.community_id = ? AND substr(e.occurred_on, 1, 7) = ?
		ORDER BY e.occurred_on DESC, e.created_at DESC`,
		communityID, ym)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EntryRow
	for rows.Next() {
		var row EntryRow
		var created int64
		if err := rows.Scan(&row.ID, &row.CommunityID, &row.ProjectID, &row.Minutes,
			&row.Note, &row.OccurredOn, &row.CreatedBy, &created,
			&row.ProjectTitle, &row.CreatorName); err != nil {
			return nil, err
		}
		row.CreatedAt = time.Unix(created, 0)
		out = append(out, row)
	}
	return out, rows.Err()
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
