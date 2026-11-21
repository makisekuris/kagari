package sqlite

import (
	"context"
	"database/sql"
)

// reminderSchema is additive so databases already at schema version 6 can
// enable reminders without rebuilding the tables that use the version-6 CHECKs.
const reminderSchema = `
CREATE TABLE IF NOT EXISTS personal_reminders (
	id INTEGER PRIMARY KEY,
	title TEXT NOT NULL,
	steps_json TEXT NOT NULL CHECK(json_valid(steps_json)),
	repeat INTEGER NOT NULL CHECK(repeat IN (0,1)),
	start_at TEXT NOT NULL,
	cron TEXT NOT NULL DEFAULT '',
	timezone TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL CHECK(status IN ('active','completed','cancelled')),
	next_run_at TEXT,
	next_step INTEGER NOT NULL CHECK(next_step>=0),
	created_event_id INTEGER NOT NULL REFERENCES personal_events(id),
	last_delivery_id INTEGER REFERENCES personal_delivery_intents(id),
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	CHECK((status='active' AND next_run_at IS NOT NULL) OR (status<>'active' AND next_run_at IS NULL))
);
CREATE INDEX IF NOT EXISTS personal_reminders_due ON personal_reminders(next_run_at,id) WHERE status='active';
CREATE INDEX IF NOT EXISTS personal_reminders_created_event ON personal_reminders(created_event_id,id);
CREATE TABLE IF NOT EXISTS personal_reminder_requests (
	event_id INTEGER NOT NULL REFERENCES personal_events(id),
	request_key TEXT NOT NULL,
	operation TEXT NOT NULL CHECK(operation IN ('create_reminder','cancel_reminder')),
	reminder_id INTEGER NOT NULL REFERENCES personal_reminders(id),
	result_json TEXT NOT NULL CHECK(json_valid(result_json)),
	payload TEXT NOT NULL CHECK(json_valid(payload)),
	PRIMARY KEY(event_id,request_key)
);
CREATE INDEX IF NOT EXISTS personal_reminder_requests_reminder ON personal_reminder_requests(reminder_id,event_id);
`

// ensureReminderColumns upgrades reminder tables made by earlier schema-6
// builds without rebuilding any version-6 tables or changing their CHECKs.
func ensureReminderColumns(ctx context.Context, tx *sql.Tx) error {
	for _, column := range []struct{ name, definition string }{
		{"cron", `TEXT NOT NULL DEFAULT ''`},
		{"timezone", `TEXT NOT NULL DEFAULT ''`},
		{"last_delivery_id", `INTEGER REFERENCES personal_delivery_intents(id)`},
	} {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('personal_reminders') WHERE name=?`, column.name).Scan(&exists); err != nil {
			return err
		}
		if exists != 0 {
			continue
		}
		statement := "ALTER TABLE personal_reminders ADD COLUMN " + column.name + " " + column.definition
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS personal_reminders_last_delivery_due
		ON personal_reminders(last_delivery_id,next_run_at) WHERE status='active' AND last_delivery_id IS NOT NULL`)
	return err
}
