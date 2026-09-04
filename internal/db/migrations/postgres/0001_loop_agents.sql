CREATE TABLE IF NOT EXISTS loop_agents (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	prompt TEXT NOT NULL,
	interval_sec BIGINT NOT NULL,
	allowed_tools TEXT NOT NULL DEFAULT '[]',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	enabled SMALLINT NOT NULL DEFAULT 0,
	last_run_at BIGINT NOT NULL DEFAULT 0,
	last_run_id TEXT NOT NULL DEFAULT '',
	last_run_cost DOUBLE PRECISION NOT NULL DEFAULT 0,
	created_at BIGINT NOT NULL,
	updated_at BIGINT NOT NULL
)
--;;
CREATE INDEX IF NOT EXISTS loop_agents_name_idx ON loop_agents (name)
