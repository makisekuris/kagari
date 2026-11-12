package sqlite

// personalSchema contains the tables shared by events, tasks, memories,
// attachments, and replies. Feature-specific schema fragments stay beside the
// code that owns them and are concatenated during NewStore.
const personalSchema = `
CREATE TABLE IF NOT EXISTS personal_schema (version INTEGER PRIMARY KEY CHECK(version=4));
INSERT OR IGNORE INTO personal_schema(version) VALUES(4);
CREATE TABLE IF NOT EXISTS personal_meta (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS personal_bindings (
	channel TEXT PRIMARY KEY,
	account TEXT NOT NULL,
	sender TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS personal_inbox (
	channel TEXT NOT NULL,
	account TEXT NOT NULL,
	cursor TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(channel,account)
);
CREATE TABLE IF NOT EXISTS personal_attachments (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	mime TEXT NOT NULL DEFAULT '',
	size INTEGER NOT NULL CHECK(size>=0 AND size<=20000000),
	path TEXT NOT NULL,
	status TEXT NOT NULL CHECK(status IN ('available','deleting','deleted')),
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS personal_tasks (
	id INTEGER PRIMARY KEY,
	title TEXT NOT NULL,
	goal TEXT NOT NULL,
	context TEXT NOT NULL,
	attachments TEXT NOT NULL DEFAULT '[]',
	revision INTEGER NOT NULL CHECK(revision>0),
	status TEXT NOT NULL CHECK(status IN ('queued','running','awaiting_review','waiting_input','waiting_budget','completed','failed','cancelled')),
	summary TEXT NOT NULL DEFAULT '',
	checkpoint TEXT NOT NULL DEFAULT '',
	source_run_ids TEXT NOT NULL DEFAULT '[]',
	memory_refs TEXT NOT NULL DEFAULT '[]',
	budget_ms INTEGER NOT NULL CHECK(budget_ms>0),
	used_ms INTEGER NOT NULL DEFAULT 0 CHECK(used_ms>=0),
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS personal_tasks_list ON personal_tasks(updated_at DESC,id DESC);
CREATE TABLE IF NOT EXISTS personal_runs (
	id INTEGER PRIMARY KEY,
	task_id INTEGER REFERENCES personal_tasks(id),
	revision INTEGER NOT NULL DEFAULT 0,
	event_id INTEGER NOT NULL REFERENCES personal_events(id),
	kind TEXT NOT NULL CHECK(kind IN ('task','memory_review')),
	input TEXT NOT NULL,
	status TEXT NOT NULL CHECK(status IN ('queued','running','completed','failed','cancelled')),
	result TEXT NOT NULL DEFAULT '',
	context_manifest TEXT NOT NULL DEFAULT '',
	memory_versions TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(memory_versions)),
	error TEXT NOT NULL DEFAULT '',
	elapsed_ms INTEGER NOT NULL DEFAULT 0 CHECK(elapsed_ms>=0),
	created_at TEXT NOT NULL,
	CHECK((kind='task' AND task_id IS NOT NULL AND revision>0) OR (kind='memory_review' AND task_id IS NULL AND revision=0))
);
CREATE INDEX IF NOT EXISTS personal_runs_claim ON personal_runs(kind,status,id);
CREATE INDEX IF NOT EXISTS personal_runs_task ON personal_runs(task_id,revision,status);
CREATE UNIQUE INDEX IF NOT EXISTS personal_runs_memory_review ON personal_runs(event_id) WHERE kind='memory_review';
CREATE TABLE IF NOT EXISTS personal_events (
	id INTEGER PRIMARY KEY,
	schedule_id INTEGER REFERENCES personal_schedules(id),
	reply_outbox_id INTEGER REFERENCES personal_outbox(id),
	kind TEXT NOT NULL CHECK(kind IN ('user','material','task','schedule')),
	text TEXT NOT NULL,
	task_id INTEGER REFERENCES personal_tasks(id),
	run_id INTEGER REFERENCES personal_runs(id),
	channel TEXT NOT NULL DEFAULT '',
	account TEXT NOT NULL DEFAULT '',
	sender TEXT NOT NULL DEFAULT '',
	message_id TEXT NOT NULL DEFAULT '',
	reply_to_id TEXT NOT NULL DEFAULT '',
	attachments TEXT NOT NULL DEFAULT '[]',
	status TEXT NOT NULL CHECK(status IN ('queued','running','completed')),
	interrupted INTEGER NOT NULL DEFAULT 0 CHECK(interrupted IN (0,1)),
	elapsed_ms INTEGER NOT NULL DEFAULT 0 CHECK(elapsed_ms>=0),
	created_at TEXT NOT NULL,
	CHECK(kind<>'schedule' OR (status='completed' AND run_id IS NULL AND schedule_id IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS personal_events_message ON personal_events(channel,account,sender,message_id) WHERE message_id<>'';
CREATE UNIQUE INDEX IF NOT EXISTS personal_events_run ON personal_events(run_id) WHERE run_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS personal_events_claim ON personal_events(status,id);
CREATE TABLE IF NOT EXISTS personal_history (
	id INTEGER PRIMARY KEY,
	schedule_id INTEGER REFERENCES personal_schedules(id),
	event_id INTEGER NOT NULL REFERENCES personal_events(id),
	role TEXT NOT NULL,
	text TEXT NOT NULL,
	attachments TEXT NOT NULL DEFAULT '[]'
);
CREATE INDEX IF NOT EXISTS personal_history_event ON personal_history(event_id,id);
CREATE TABLE IF NOT EXISTS personal_requests (
	event_id INTEGER NOT NULL REFERENCES personal_events(id),
	request_key TEXT NOT NULL,
	operation TEXT NOT NULL,
	payload TEXT NOT NULL,
	task_id INTEGER REFERENCES personal_tasks(id),
	run_id INTEGER REFERENCES personal_runs(id),
	memory_history_id INTEGER REFERENCES personal_memory_history(id),
	schedule_id INTEGER REFERENCES personal_schedules(id),
	result_json TEXT,
	CHECK((task_id IS NOT NULL AND run_id IS NOT NULL AND memory_history_id IS NULL AND schedule_id IS NULL) OR
		(task_id IS NULL AND run_id IS NULL AND memory_history_id IS NOT NULL AND schedule_id IS NULL) OR
		(task_id IS NULL AND run_id IS NULL AND memory_history_id IS NULL AND schedule_id IS NOT NULL AND result_json IS NOT NULL)),
	PRIMARY KEY(event_id,request_key)
);
CREATE TABLE IF NOT EXISTS personal_memories (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL,
	version INTEGER NOT NULL CHECK(version>0),
	source_event_id INTEGER NOT NULL REFERENCES personal_events(id),
	deleted INTEGER NOT NULL CHECK(deleted IN (0,1)),
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS personal_memories_source ON personal_memories(source_event_id);
CREATE TABLE IF NOT EXISTS personal_memory_history (
	id INTEGER PRIMARY KEY,
	key TEXT NOT NULL,
	value TEXT NOT NULL,
	version INTEGER NOT NULL CHECK(version>0),
	source_event_id INTEGER NOT NULL REFERENCES personal_events(id),
	run_id INTEGER REFERENCES personal_runs(id),
	deleted INTEGER NOT NULL CHECK(deleted IN (0,1)),
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS personal_memory_history_key ON personal_memory_history(key,version DESC);
CREATE INDEX IF NOT EXISTS personal_memory_history_recent ON personal_memory_history(id DESC);
CREATE TABLE IF NOT EXISTS personal_outbox (
	id INTEGER PRIMARY KEY,
	suggestion_id INTEGER REFERENCES personal_schedule_context_suggestions(id),
	event_id INTEGER NOT NULL REFERENCES personal_events(id),
	part INTEGER NOT NULL CHECK(part>0),
	text TEXT NOT NULL,
	attachment_id TEXT REFERENCES personal_attachments(id),
	status TEXT NOT NULL CHECK(status IN ('pending','sending','sent','failed','uncertain')),
	attempts INTEGER NOT NULL DEFAULT 0,
	next_attempt_at TEXT NOT NULL,
	receipt_channel TEXT NOT NULL DEFAULT '',
	receipt_account TEXT NOT NULL DEFAULT '',
	receipt_conversation TEXT NOT NULL DEFAULT '',
	receipt_message_id TEXT NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT '',
	UNIQUE(event_id,part)
);
CREATE INDEX IF NOT EXISTS personal_outbox_claim ON personal_outbox(status,next_attempt_at,id);
`
