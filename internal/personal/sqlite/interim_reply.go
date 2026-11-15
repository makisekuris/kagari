package sqlite

import (
	"context"
	"errors"

	"kagari/internal/personal"
)

// QueueInterimReply persists visible model text without completing its user event.
// Rendered deliveries do not become recovery cursors for unfinished task actions.
func (s *Store) QueueInterimReply(ctx context.Context, eventID int64, text string, chunks []string) error {
	if err := validatePreparedText("interim reply", text, chunks); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var kind personal.EventKind
	var status personal.EventStatus
	if err := tx.QueryRowContext(ctx, `SELECT kind,status FROM personal_events WHERE id=?`, eventID).Scan(&kind, &status); err != nil {
		return err
	}
	if kind != personal.EventUser || status != personal.EventRunning {
		return errors.New("personal: interim reply requires a running user event")
	}
	_, _, err = s.appendSupplementalDeliveryTx(ctx, tx, eventID, personal.SupplementalReply{
		Draft: personal.DeliveryDraft{Text: text, Attachments: []string{}}, Purpose: personal.DeliveryResult, Chunks: chunks,
	})
	if err != nil {
		return err
	}
	return tx.Commit()
}
