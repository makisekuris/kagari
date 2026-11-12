package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"kagari/internal/personal"
)

// Store serializes operations through one SQLite connection, including claims and transactions.
type Store struct {
	db           *database
	taskBudgetMS int64
}

var (
	_ personal.TaskInputProcessingRepository = (*Store)(nil)
	_ personal.ProgressRepository            = (*Store)(nil)
)

func NewStore(path string, taskBudget time.Duration) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("personal: database path is required")
	}
	budgetMS := int64(taskBudget / time.Millisecond)
	if budgetMS <= 0 {
		return nil, errors.New("personal: task budget must be at least one millisecond")
	}
	db, err := openDatabase(context.Background(), path)
	if err != nil {
		return nil, err
	}
	if err := checkPersonalSchema(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("personal: begin schema transaction: %w", err)
	}
	if _, err := tx.Exec(personalSchema + scheduleSchema + scheduleSuggestionSchema + replySchema + taskInputSchema + materialSchema + progressSchema); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("personal: initialize schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("personal: commit schema: %w", err)
	}
	return &Store{db: db, taskBudgetMS: budgetMS}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func recordNotFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return personal.ErrRecordNotFound
	}
	return err
}

type schemaQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func checkPersonalSchema(ctx context.Context, db schemaQueryer) error {
	var marker int
	err := db.QueryRowContext(ctx, `SELECT version FROM personal_schema LIMIT 1`).Scan(&marker)
	if err == nil {
		if marker != 4 {
			return fmt.Errorf("personal: unsupported database schema version %d", marker)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		var hasTable int
		if tableErr := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name LIKE 'personal_%'`).Scan(&hasTable); tableErr != nil {
			return tableErr
		}
		if hasTable > 0 {
			return errors.New("personal: unsupported database schema; existing personal data was left untouched")
		}
	}
	return nil
}

func timestamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

func parseTimestamp(raw string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("personal: invalid stored timestamp: %w", err)
	}
	return t, nil
}

func remainingMS(budget, used int64) int64 {
	if used >= budget {
		return 0
	}
	return budget - used
}
