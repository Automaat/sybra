CREATE TABLE IF NOT EXISTS task_attachments (
	task_id TEXT NOT NULL,
	id TEXT NOT NULL,
	file_name TEXT NOT NULL DEFAULT '',
	content_type TEXT NOT NULL DEFAULT '',
	size_bytes INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL DEFAULT 0,
	content BLOB NOT NULL,
	PRIMARY KEY (task_id, id)
)
--;;
CREATE INDEX IF NOT EXISTS task_attachments_task_idx ON task_attachments (task_id, created_at)
--;;
CREATE TABLE IF NOT EXISTS task_artifacts (
	task_id TEXT NOT NULL,
	name TEXT NOT NULL,
	kind TEXT NOT NULL DEFAULT '',
	size_bytes INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL DEFAULT 0,
	content BLOB NOT NULL,
	PRIMARY KEY (task_id, name)
)
--;;
CREATE INDEX IF NOT EXISTS task_artifacts_task_idx ON task_artifacts (task_id, name)
