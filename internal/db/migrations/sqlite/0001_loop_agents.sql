CREATE TABLE IF NOT EXISTS loop_agents (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	prompt TEXT NOT NULL,
	interval_sec INTEGER NOT NULL,
	allowed_tools TEXT NOT NULL DEFAULT '[]',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	enabled INTEGER NOT NULL DEFAULT 0,
	last_run_at INTEGER NOT NULL DEFAULT 0,
	last_run_id TEXT NOT NULL DEFAULT '',
	last_run_cost REAL NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
)
--;;
CREATE INDEX IF NOT EXISTS loop_agents_name_idx ON loop_agents (name)
