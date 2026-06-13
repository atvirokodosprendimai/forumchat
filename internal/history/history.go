package history

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

type Handler struct {
	DB            *sql.DB
	CommunityID   string
	CommunityName string
	Log           *slog.Logger
}

func (h *Handler) viewer(r *http.Request) webtempl.Viewer {
	v := webtempl.Viewer{CommunityName: h.CommunityName}
	if id, ok := auth.FromContext(r.Context()); ok {
		v.IsAuthed = true
		v.DisplayName = id.Membership.DisplayName
		v.Role = string(id.Membership.Role)
	}
	return v
}

// GetIndex renders the history page. Query params:
//
//	d=YYYY-MM-DD  selected day (default: today, local server time)
//	m=YYYY-MM     calendar month being browsed (default: month of d)
func (h *Handler) GetIndex(w http.ResponseWriter, r *http.Request) {
	loc := time.Local
	now := time.Now().In(loc)

	day := parseDay(r.URL.Query().Get("d"), now, loc)
	month := parseMonth(r.URL.Query().Get("m"), day, loc)

	monthStart := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, loc)
	monthEnd := monthStart.AddDate(0, 1, 0)

	activeDays, err := h.daysWithActivity(r.Context(), monthStart, monthEnd)
	if err != nil {
		http.Error(w, "load activity: "+err.Error(), http.StatusInternalServerError)
		return
	}

	dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	dayEnd := dayStart.AddDate(0, 0, 1)
	events, err := h.eventsBetween(r.Context(), dayStart, dayEnd)
	if err != nil {
		http.Error(w, "load events: "+err.Error(), http.StatusInternalServerError)
		return
	}

	data := webtempl.HistoryPageData{
		Viewer:       h.viewer(r),
		SelectedDay:  dayStart,
		CalendarBase: monthStart,
		Calendar:     buildCalendar(monthStart, activeDays, dayStart),
		PrevMonth:    monthStart.AddDate(0, -1, 0),
		NextMonth:    monthStart.AddDate(0, 1, 0),
		PrevDay:      dayStart.AddDate(0, 0, -1),
		NextDay:      dayStart.AddDate(0, 0, 1),
		Today:        time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc),
		Events:       events,
	}
	_ = webtempl.HistoryPage(data).Render(r.Context(), w)
}

func parseDay(s string, fallback time.Time, loc *time.Location) time.Time {
	if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return t
	}
	return fallback
}

func parseMonth(s string, fallback time.Time, loc *time.Location) time.Time {
	if t, err := time.ParseInLocation("2006-01", s, loc); err == nil {
		return t
	}
	return fallback
}

// daysWithActivity returns the set of YYYY-MM-DD strings (local TZ) that have
// at least one non-deleted chat message, thread, or post created in
// [monthStart, monthEnd).
func (h *Handler) daysWithActivity(ctx context.Context, monthStart, monthEnd time.Time) (map[string]struct{}, error) {
	loc := monthStart.Location()
	from := monthStart.Unix()
	to := monthEnd.Unix()

	out := map[string]struct{}{}
	add := func(unix int64) {
		out[time.Unix(unix, 0).In(loc).Format("2006-01-02")] = struct{}{}
	}

	q := func(query string, args ...any) error {
		rows, err := h.DB.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t int64
			if err := rows.Scan(&t); err != nil {
				return err
			}
			add(t)
		}
		return rows.Err()
	}

	if err := q(`SELECT created_at FROM chat_messages
		WHERE community_id = ? AND deleted_at IS NULL AND created_at >= ? AND created_at < ?`,
		h.CommunityID, from, to); err != nil {
		return nil, err
	}
	if err := q(`SELECT created_at FROM threads
		WHERE community_id = ? AND deleted_at IS NULL AND created_at >= ? AND created_at < ?`,
		h.CommunityID, from, to); err != nil {
		return nil, err
	}
	if err := q(`SELECT p.created_at FROM posts p
		JOIN threads t ON t.id = p.thread_id
		WHERE t.community_id = ? AND p.deleted_at IS NULL AND p.created_at >= ? AND p.created_at < ?`,
		h.CommunityID, from, to); err != nil {
		return nil, err
	}
	return out, nil
}

// eventsBetween returns chat messages, new threads, and forum replies created
// in [from, to), merged and sorted ascending by created_at.
func (h *Handler) eventsBetween(ctx context.Context, from, to time.Time) ([]webtempl.HistoryEvent, error) {
	var events []webtempl.HistoryEvent

	// Chat messages.
	{
		rows, err := h.DB.QueryContext(ctx, `
			SELECT m.id, m.body_html, m.kind, m.created_at,
			       COALESCE(mb.display_name, '(unknown)')
			FROM chat_messages m
			LEFT JOIN memberships mb ON mb.user_id = m.author_id AND mb.community_id = m.community_id
			WHERE m.community_id = ? AND m.deleted_at IS NULL
			  AND m.created_at >= ? AND m.created_at < ?`,
			h.CommunityID, from.Unix(), to.Unix())
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var (
				id, bodyHTML, kind, author string
				ts                         int64
			)
			if err := rows.Scan(&id, &bodyHTML, &kind, &ts, &author); err != nil {
				rows.Close()
				return nil, err
			}
			events = append(events, webtempl.HistoryEvent{
				Source:     webtempl.HistorySourceChat,
				CreatedAt:  time.Unix(ts, 0),
				AuthorName: author,
				BodyHTML:   bodyHTML,
				Link:       "/chat",
				Kind:       kind,
			})
		}
		rows.Close()
	}

	// New threads.
	{
		rows, err := h.DB.QueryContext(ctx, `
			SELECT t.id, t.subject, t.body_html, t.created_at,
			       COALESCE(mb.display_name, '(unknown)')
			FROM threads t
			LEFT JOIN memberships mb ON mb.user_id = t.author_id AND mb.community_id = t.community_id
			WHERE t.community_id = ? AND t.deleted_at IS NULL
			  AND t.created_at >= ? AND t.created_at < ?`,
			h.CommunityID, from.Unix(), to.Unix())
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var (
				id, subject, bodyHTML, author string
				ts                            int64
			)
			if err := rows.Scan(&id, &subject, &bodyHTML, &ts, &author); err != nil {
				rows.Close()
				return nil, err
			}
			events = append(events, webtempl.HistoryEvent{
				Source:        webtempl.HistorySourceThread,
				CreatedAt:     time.Unix(ts, 0),
				AuthorName:    author,
				Title:         subject,
				BodyHTML:      bodyHTML,
				Link:          "/forum/" + id,
				ThreadSubject: subject,
			})
		}
		rows.Close()
	}

	// Forum replies.
	{
		rows, err := h.DB.QueryContext(ctx, `
			SELECT p.id, p.thread_id, p.body_html, p.created_at,
			       COALESCE(mb.display_name, '(unknown)'), t.subject
			FROM posts p
			JOIN threads t ON t.id = p.thread_id
			LEFT JOIN memberships mb ON mb.user_id = p.author_id AND mb.community_id = t.community_id
			WHERE t.community_id = ? AND p.deleted_at IS NULL
			  AND p.created_at >= ? AND p.created_at < ?`,
			h.CommunityID, from.Unix(), to.Unix())
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var (
				id, threadID, bodyHTML, author, subject string
				ts                                      int64
			)
			if err := rows.Scan(&id, &threadID, &bodyHTML, &ts, &author, &subject); err != nil {
				rows.Close()
				return nil, err
			}
			events = append(events, webtempl.HistoryEvent{
				Source:        webtempl.HistorySourceReply,
				CreatedAt:     time.Unix(ts, 0),
				AuthorName:    author,
				BodyHTML:      bodyHTML,
				Link:          "/forum/" + threadID + "#post-" + id,
				ThreadSubject: subject,
			})
		}
		rows.Close()
	}

	sort.Slice(events, func(i, j int) bool { return events[i].CreatedAt.Before(events[j].CreatedAt) })
	return events, nil
}

// buildCalendar lays out a month-grid starting from the Monday of the week
// containing the first of the month, through the Sunday of the week
// containing the last of the month.
func buildCalendar(monthStart time.Time, activeDays map[string]struct{}, selected time.Time) []webtempl.HistoryWeek {
	loc := monthStart.Location()
	// Find Monday on/before monthStart.
	wd := int(monthStart.Weekday()) // Sun=0..Sat=6
	mondayOffset := (wd + 6) % 7    // Mon=0..Sun=6
	gridStart := monthStart.AddDate(0, 0, -mondayOffset)

	monthEnd := monthStart.AddDate(0, 1, 0)
	lastDay := monthEnd.AddDate(0, 0, -1)
	wd = int(lastDay.Weekday())
	sundayOffset := (7 - wd) % 7
	gridEnd := lastDay.AddDate(0, 0, sundayOffset)

	weeks := make([]webtempl.HistoryWeek, 0, 6)
	cur := gridStart
	selectedKey := selected.Format("2006-01-02")
	for !cur.After(gridEnd) {
		week := webtempl.HistoryWeek{}
		for i := 0; i < 7; i++ {
			d := cur.AddDate(0, 0, i)
			key := d.Format("2006-01-02")
			_, has := activeDays[key]
			week.Days[i] = webtempl.HistoryDay{
				Date:       d,
				DateKey:    key,
				InMonth:    d.Month() == monthStart.Month(),
				HasActive:  has,
				IsSelected: key == selectedKey,
			}
		}
		weeks = append(weeks, week)
		cur = time.Date(cur.Year(), cur.Month(), cur.Day()+7, 0, 0, 0, 0, loc)
	}
	return weeks
}

