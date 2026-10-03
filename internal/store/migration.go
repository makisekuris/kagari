package store

import (
	"context"
	"database/sql"
)

func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	jobColumns, err := tableColumns(ctx, tx, "jobs")
	if err != nil {
		return err
	}
	if !jobColumns["targets"] {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE jobs ADD COLUMN targets TEXT`); err != nil {
			return err
		}
	}
	deliveryColumns, err := tableColumns(ctx, tx, "deliveries")
	if err != nil {
		return err
	}
	if !deliveryColumns["channel"] || !deliveryColumns["address"] {
		if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS deliveries_claim`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS deliveries_order`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE deliveries_new (
			id INTEGER PRIMARY KEY,
			job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
			channel TEXT NOT NULL DEFAULT 'telegram',
			address TEXT NOT NULL DEFAULT '',
			chat_id INTEGER NOT NULL DEFAULT 0,
			part INTEGER NOT NULL,
			text TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('pending','sending','sent','failed','uncertain')),
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			next_attempt_at TEXT NOT NULL,
			message_id TEXT,
			UNIQUE (job_id, channel, address, part)
		)`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO deliveries_new(id,job_id,channel,address,chat_id,part,text,status,attempts,last_error,next_attempt_at,message_id)
			SELECT id,job_id,'telegram',CAST(chat_id AS TEXT),chat_id,part,text,status,attempts,last_error,next_attempt_at,message_id FROM deliveries`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DROP TABLE deliveries`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE deliveries_new RENAME TO deliveries`); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS deliveries_claim ON deliveries(status, next_attempt_at, id)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS deliveries_order ON deliveries(job_id, channel, address, part, status)`); err != nil {
		return err
	}
	return tx.Commit()
}

func tableColumns(ctx context.Context, tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, dataType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}
