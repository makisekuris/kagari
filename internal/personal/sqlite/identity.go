package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
)

// EnsureIdentity runs under the service lock. A database replacement must not
// reuse old log associations merely because its integer event IDs start at one.
func (s *Store) EnsureIdentity(ctx context.Context) (string, error) {
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO personal_meta(key,value) VALUES('store_id',?)`, rand.Text()); err != nil {
		return "", err
	}
	return s.Identity(ctx)
}

// Identity does not initialize metadata, so inspection also works before the
// first observable service startup without changing the database.
func (s *Store) Identity(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM personal_meta WHERE key='store_id'`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}
