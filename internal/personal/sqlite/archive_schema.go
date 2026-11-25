package sqlite

const archiveSchema = `
CREATE TABLE IF NOT EXISTS personal_archive_documents (
	id INTEGER PRIMARY KEY,
	title TEXT NOT NULL,
	category TEXT NOT NULL DEFAULT '',
	tags TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(tags)),
	latest_version_id INTEGER REFERENCES personal_archive_versions(id),
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS personal_archive_documents_category ON personal_archive_documents(category,id);
CREATE INDEX IF NOT EXISTS personal_archive_documents_latest ON personal_archive_documents(latest_version_id);
CREATE TABLE IF NOT EXISTS personal_archive_versions (
	id INTEGER PRIMARY KEY,
	document_id INTEGER NOT NULL REFERENCES personal_archive_documents(id),
	number INTEGER NOT NULL CHECK(number>0),
	historical INTEGER NOT NULL CHECK(historical IN (0,1)),
	attachment_id TEXT NOT NULL REFERENCES personal_attachments(id),
	name TEXT NOT NULL,
	mime TEXT NOT NULL DEFAULT '',
	size INTEGER NOT NULL CHECK(size>=0),
	object_key TEXT NOT NULL UNIQUE,
	event_id INTEGER NOT NULL REFERENCES personal_events(id),
	source_event_id INTEGER NOT NULL REFERENCES personal_events(id),
	submitted_at TEXT NOT NULL,
	note TEXT NOT NULL DEFAULT '',
	upload_status TEXT NOT NULL CHECK(upload_status IN ('pending','processing','ready','failed')),
	summary_status TEXT NOT NULL CHECK(summary_status IN ('pending','processing','ready','failed','needs_note')),
	summary TEXT NOT NULL DEFAULT '',
	summary_source TEXT NOT NULL DEFAULT '' CHECK(summary_source IN ('','original','owner_note')),
	summary_scope TEXT NOT NULL DEFAULT '',
	extracted_text TEXT NOT NULL DEFAULT '',
	extracted_partial INTEGER NOT NULL DEFAULT 0 CHECK(extracted_partial IN (0,1)),
	extracted_scope TEXT NOT NULL DEFAULT '',
	upload_error TEXT NOT NULL DEFAULT '',
	summary_error TEXT NOT NULL DEFAULT '',
	UNIQUE(document_id,number)
);
CREATE INDEX IF NOT EXISTS personal_archive_versions_document ON personal_archive_versions(document_id,number DESC,id DESC);
CREATE INDEX IF NOT EXISTS personal_archive_versions_submission ON personal_archive_versions(document_id,historical,id DESC);
CREATE INDEX IF NOT EXISTS personal_archive_versions_submitted ON personal_archive_versions(submitted_at,id DESC);
CREATE INDEX IF NOT EXISTS personal_archive_versions_attachment ON personal_archive_versions(attachment_id);
CREATE INDEX IF NOT EXISTS personal_archive_versions_upload ON personal_archive_versions(upload_status,id);
CREATE INDEX IF NOT EXISTS personal_archive_versions_summary ON personal_archive_versions(summary_status,id);
CREATE TABLE IF NOT EXISTS personal_archive_requests (
	event_id INTEGER NOT NULL REFERENCES personal_events(id),
	request_key TEXT NOT NULL,
	payload TEXT NOT NULL CHECK(json_valid(payload)),
	version_id INTEGER NOT NULL REFERENCES personal_archive_versions(id),
	PRIMARY KEY(event_id,request_key)
);
CREATE INDEX IF NOT EXISTS personal_archive_requests_version ON personal_archive_requests(version_id);
CREATE TABLE IF NOT EXISTS personal_archive_work (
	id INTEGER PRIMARY KEY,
	version_id INTEGER NOT NULL REFERENCES personal_archive_versions(id),
	kind TEXT NOT NULL CHECK(kind IN ('upload','summary')),
	status TEXT NOT NULL CHECK(status IN ('queued','processing','retry','done','failed')),
	attempt INTEGER NOT NULL DEFAULT 0 CHECK(attempt>=0),
	next_attempt_at TEXT NOT NULL,
	last_error TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL,
	UNIQUE(version_id,kind)
);
CREATE INDEX IF NOT EXISTS personal_archive_work_claim ON personal_archive_work(status,next_attempt_at,id);
CREATE INDEX IF NOT EXISTS personal_archive_work_version ON personal_archive_work(version_id,kind,status);
`
