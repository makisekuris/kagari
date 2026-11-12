package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"kagari/internal/personal"
)

// RunMemory freezes the first observed version of a key for one active Run.
func (s *Store) RunMemory(ctx context.Context, runID int64, key string) (*personal.Memory, error) {
	if runID <= 0 {
		return nil, errors.New("personal: run memory requires a positive run ID")
	}
	if !validMemoryKey(key) {
		return nil, errors.New("personal: invalid memory key")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := activeRunTx(ctx, tx, runID); err != nil {
		return nil, err
	}
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT memory_versions FROM personal_runs WHERE id=?`, runID).Scan(&raw); err != nil {
		return nil, err
	}
	versions := map[string]int64{}
	if err := json.Unmarshal([]byte(raw), &versions); err != nil {
		return nil, fmt.Errorf("personal: decode Run memory versions: %w", err)
	}
	if versions == nil {
		versions = map[string]int64{}
	}
	version, frozen := versions[key]
	if version < 0 {
		return nil, errors.New("personal: invalid frozen memory version")
	}
	var current *personal.Memory
	if !frozen {
		current, err = scanMemory(tx.QueryRowContext(ctx, `SELECT key,value,version,source_event_id,deleted,updated_at FROM personal_memories WHERE key=?`, key))
		if errors.Is(err, sql.ErrNoRows) {
			err = nil
		} else if err != nil {
			return nil, err
		}
		if current != nil {
			version = current.Version
		}
		versions[key] = version
		encoded, err := json.Marshal(versions)
		if err != nil {
			return nil, fmt.Errorf("personal: encode Run memory versions: %w", err)
		}
		result, err := tx.ExecContext(ctx, `UPDATE personal_runs SET memory_versions=? WHERE id=? AND status='running'`, string(encoded), runID)
		if err != nil {
			return nil, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if changed != 1 {
			return nil, errors.New("personal: active Run changed while freezing memory")
		}
	} else if version > 0 {
		current, err = scanMemory(tx.QueryRowContext(ctx, `SELECT key,value,version,source_event_id,deleted,updated_at FROM personal_memories WHERE key=?`, key))
		if errors.Is(err, sql.ErrNoRows) {
			current, err = nil, nil
		}
		if err != nil {
			return nil, err
		}
	}

	var selected *personal.Memory
	if version > 0 && current != nil && !current.Deleted {
		var forgotten int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM personal_memory_history WHERE key=? AND version>? AND deleted=1
		)`, key, version).Scan(&forgotten); err != nil {
			return nil, err
		}
		if forgotten == 0 {
			selected, err = scanMemory(tx.QueryRowContext(ctx, `SELECT key,value,version,source_event_id,deleted,updated_at
				FROM personal_memory_history WHERE key=? AND version=?`, key, version))
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return nil, errors.New("personal: frozen memory version is missing from history")
				}
				return nil, err
			}
			if selected.Deleted {
				selected = nil
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return selected, nil
}
