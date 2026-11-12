package sqlite

const scheduleSchema = `
CREATE TABLE IF NOT EXISTS personal_schedules (
	id INTEGER PRIMARY KEY,
	revision INTEGER NOT NULL CHECK(revision>0),
	scope_revision INTEGER NOT NULL CHECK(scope_revision>0),
	title TEXT NOT NULL,
	goal TEXT NOT NULL,
	context TEXT NOT NULL,
	attachments TEXT NOT NULL DEFAULT '[]',
	window_mode TEXT NOT NULL CHECK(window_mode IN ('none','since_success')),
	required_sources TEXT NOT NULL DEFAULT '[]',
	cron TEXT NOT NULL,
	timezone TEXT NOT NULL,
	status TEXT NOT NULL CHECK(status IN ('active','paused','cancelled')),
	next_run_at TEXT,
	pause_reason TEXT NOT NULL DEFAULT '',
	task_budget_ms INTEGER NOT NULL CHECK(task_budget_ms>0),
	baseline_at TEXT,
	covered_through TEXT,
	created_event_id INTEGER NOT NULL REFERENCES personal_events(id),
	authorized_event_id INTEGER NOT NULL REFERENCES personal_events(id),
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	CHECK((status='active' AND next_run_at IS NOT NULL) OR (status<>'active' AND next_run_at IS NULL)),
	CHECK((status='paused' AND pause_reason IN ('manual','invalid_schedule','attachment_unavailable')) OR (status<>'paused' AND pause_reason='')),
	CHECK(window_mode<>'none' OR (baseline_at IS NULL AND covered_through IS NULL)),
	CHECK(status<>'active' OR window_mode<>'since_success' OR baseline_at IS NOT NULL),
	CHECK(covered_through IS NULL OR (baseline_at IS NOT NULL AND covered_through>=baseline_at))
);
CREATE INDEX IF NOT EXISTS personal_schedules_due ON personal_schedules(next_run_at,id) WHERE status='active';
CREATE TABLE IF NOT EXISTS personal_schedule_occurrences (
	id INTEGER PRIMARY KEY,
	schedule_id INTEGER NOT NULL REFERENCES personal_schedules(id),
	schedule_revision INTEGER NOT NULL CHECK(schedule_revision>0),
	scope_revision INTEGER NOT NULL CHECK(scope_revision>0),
	scheduled_at TEXT NOT NULL,
	checked_at TEXT NOT NULL,
	coalesced_until TEXT,
	outcome TEXT NOT NULL CHECK(outcome IN ('enqueued','skipped_busy','blocked')),
	reason TEXT NOT NULL DEFAULT '',
	task_id INTEGER UNIQUE REFERENCES personal_tasks(id),
	event_id INTEGER UNIQUE REFERENCES personal_events(id),
	definition_json TEXT NOT NULL,
	started_at TEXT,
	window_start TEXT,
	window_end TEXT,
	coverage_eligible INTEGER NOT NULL DEFAULT 1 CHECK(coverage_eligible IN (0,1)),
	check_result_json TEXT,
	credited_task_revision INTEGER,
	credited_at TEXT,
	UNIQUE(schedule_id,scheduled_at),
	CHECK((outcome='enqueued' AND task_id IS NOT NULL AND event_id IS NOT NULL) OR (outcome<>'enqueued' AND task_id IS NULL AND event_id IS NULL)),
	CHECK((window_start IS NULL AND window_end IS NULL) OR (window_start IS NOT NULL AND window_end IS NOT NULL AND window_start<window_end AND started_at IS NOT NULL AND window_end=started_at)),
	CHECK(credited_at IS NULL OR (check_result_json IS NOT NULL AND credited_task_revision>0))
);
CREATE INDEX IF NOT EXISTS personal_schedule_occurrences_list ON personal_schedule_occurrences(schedule_id,id DESC);
`
