package sqlite

import (
	"context"
	"encoding/json"
	"errors"

	"kagari/internal/personal"
)

func (s *Store) ReadCheckpoints(ctx context.Context, runID int64) ([]personal.ReadCheckpoint, error) {
	run, err := activeRunTx(ctx, s.db, runID)
	if err != nil {
		return nil, recordNotFound(err)
	}
	return s.readingCheckpoints(ctx, *run)
}

func (s *Store) SaveContextManifest(ctx context.Context, runID int64, manifest personal.ContextManifest) error {
	raw, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	run, err := activeRunTx(ctx, tx, runID)
	if err != nil {
		return recordNotFound(err)
	}
	if run.Revision != manifest.Revision || manifest.FormatVersion != 1 {
		return errors.New("personal: invalid context manifest")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_runs SET context_manifest=? WHERE id=?`, string(raw), runID); err != nil {
		return err
	}
	return tx.Commit()
}
