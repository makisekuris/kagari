package sqlite

const deliverySchema = `
CREATE TABLE IF NOT EXISTS personal_delivery_intents (
	id INTEGER PRIMARY KEY,
	event_id INTEGER NOT NULL REFERENCES personal_events(id),
	schedule_id INTEGER REFERENCES personal_schedules(id),
	original_delivery_id INTEGER REFERENCES personal_delivery_intents(id),
	purpose TEXT NOT NULL CHECK(purpose IN ('result','status','control','historical')),
	draft_text TEXT NOT NULL,
	draft_attachments TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(draft_attachments)),
	state TEXT NOT NULL CHECK(state IN ('pending','rendered','suppressed')),
	detail TEXT NOT NULL DEFAULT '',
	settlement_task_id INTEGER REFERENCES personal_tasks(id),
	settlement_revision INTEGER,
	settlement_payload_json TEXT NOT NULL DEFAULT '',
	settlement_result_json TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	CHECK((settlement_task_id IS NULL AND settlement_revision IS NULL AND settlement_payload_json='' AND settlement_result_json='') OR
		(settlement_task_id IS NOT NULL AND settlement_revision IS NOT NULL AND settlement_revision>0 AND settlement_payload_json<>'' AND settlement_result_json<>'')),
	UNIQUE(event_id,settlement_task_id,settlement_revision)
);
CREATE INDEX IF NOT EXISTS personal_delivery_intents_event ON personal_delivery_intents(event_id,state,id);
CREATE INDEX IF NOT EXISTS personal_delivery_intents_task ON personal_delivery_intents(settlement_task_id,settlement_revision,id);
CREATE TABLE IF NOT EXISTS personal_delivery_scopes (
	delivery_id INTEGER NOT NULL REFERENCES personal_delivery_intents(id) ON DELETE CASCADE,
	ordinal INTEGER NOT NULL CHECK(ordinal>=0),
	task_id INTEGER NOT NULL REFERENCES personal_tasks(id),
	revision INTEGER NOT NULL CHECK(revision>0),
	run_id INTEGER REFERENCES personal_runs(id),
	PRIMARY KEY(delivery_id,ordinal)
);
CREATE INDEX IF NOT EXISTS personal_delivery_scopes_task ON personal_delivery_scopes(task_id,revision,run_id,delivery_id);
`
