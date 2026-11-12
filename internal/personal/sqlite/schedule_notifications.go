package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

type scheduleNotice struct {
	suggestionID int64
	automatic    bool
	text         string
}

// Both automatic proposals and refreshed confirmations show the stored replacement verbatim.
func scheduleNotices(ctx context.Context, tx *sql.Tx, eventID int64, kind personal.EventKind) ([]scheduleNotice, error) {
	where := `origin_event_id=? AND notified_event_id IS NULL`
	if kind == personal.EventUser {
		where = `presented_event_id=?`
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+suggestionColumns+` FROM personal_schedule_context_suggestions
		WHERE status='pending' AND `+where+` ORDER BY id`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var suggestions []personal.ScheduleSuggestion
	for rows.Next() {
		s, err := scanScheduleSuggestion(rows)
		if err != nil {
			return nil, err
		}
		suggestions = append(suggestions, *s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var notices []scheduleNotice
	for _, s := range suggestions {
		var revision int64
		var current string
		if err := tx.QueryRowContext(ctx, `SELECT revision,context FROM personal_schedules WHERE id=? AND status<>'cancelled'`, s.ScheduleID).Scan(&revision, &current); err != nil {
			if err == sql.ErrNoRows {
				continue
			}
			return nil, err
		}
		if revision != s.ScheduleRevision || current != s.OldContext {
			continue
		}
		valid := true
		for _, ref := range s.MemoryRefs {
			var version int64
			var deleted bool
			if err := tx.QueryRowContext(ctx, `SELECT version,deleted FROM personal_memories WHERE key=?`, ref.Key).Scan(&version, &deleted); err != nil {
				return nil, err
			}
			if deleted || version != ref.Version {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		var text strings.Builder
		fmt.Fprintf(&text, "建议 #%d（版本 %d）· 计划 #%d：\n", s.ID, s.Revision, s.ScheduleID)
		for _, r := range s.Replacements {
			fmt.Fprintf(&text, "原背景：%s\n建议改为：%s\n", r.Old, r.New)
		}
		text.WriteString("是否同步此计划背景？可回复建议编号并选择同步或保留。确认前计划背景不变。")
		notices = append(notices, scheduleNotice{suggestionID: s.ID, automatic: kind == personal.EventTask, text: text.String()})
	}
	return notices, nil
}

func enqueueScheduleNotices(ctx context.Context, tx *sql.Tx, eventID int64, part int, notices []scheduleNotice, now string) (int, error) {
	for _, n := range notices {
		chunks, err := channel.PrepareText(n.text)
		if err != nil {
			return part, err
		}
		for _, chunk := range chunks {
			part++
			if _, err := tx.ExecContext(ctx, `INSERT INTO personal_outbox(event_id,part,text,suggestion_id,status,next_attempt_at) VALUES(?,?,?,?,'pending',?)`, eventID, part, chunk, n.suggestionID, now); err != nil {
				return part, err
			}
		}
		if n.automatic {
			if _, err := tx.ExecContext(ctx, `UPDATE personal_schedule_context_suggestions SET notified_event_id=?,presented_event_id=? WHERE id=? AND notified_event_id IS NULL`, eventID, eventID, n.suggestionID); err != nil {
				return part, err
			}
		}
	}
	return part, nil
}
