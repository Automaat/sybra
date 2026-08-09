CREATE TABLE IF NOT EXISTS schema_imports (
	domain TEXT PRIMARY KEY,
	imported_at INTEGER NOT NULL,
	record_count INTEGER NOT NULL DEFAULT 0
)
--;;
CREATE TABLE IF NOT EXISTS experience_records (
	project_key TEXT NOT NULL,
	record_id TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	doc TEXT NOT NULL,
	PRIMARY KEY (project_key, record_id)
)
--;;
CREATE INDEX IF NOT EXISTS experience_records_query_idx
	ON experience_records (project_key, created_at DESC, record_id)
--;;
CREATE TABLE IF NOT EXISTS workflow_definitions (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL DEFAULT '',
	builtin INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	doc TEXT NOT NULL
)
--;;
CREATE TABLE IF NOT EXISTS workflow_snapshots (
	workflow_id TEXT NOT NULL,
	hash TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	doc TEXT NOT NULL,
	PRIMARY KEY (workflow_id, hash)
)
--;;
CREATE TABLE IF NOT EXISTS background_operations (
	id TEXT PRIMARY KEY,
	kind TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT '',
	started_at INTEGER NOT NULL DEFAULT 0,
	doc TEXT NOT NULL
)
