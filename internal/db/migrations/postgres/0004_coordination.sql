CREATE TABLE IF NOT EXISTS attempt_leases (
	id TEXT PRIMARY KEY,
	task_id TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT '',
	expires_at BIGINT NOT NULL DEFAULT 0,
	doc TEXT NOT NULL
)
--;;
CREATE INDEX IF NOT EXISTS attempt_leases_expiry_idx ON attempt_leases (expires_at)
--;;
CREATE INDEX IF NOT EXISTS attempt_leases_task_idx ON attempt_leases (task_id)
--;;
CREATE TABLE IF NOT EXISTS ledger_revisions (
	ledger TEXT PRIMARY KEY,
	revision BIGINT NOT NULL DEFAULT 0
)
--;;
CREATE TABLE IF NOT EXISTS agent_queue_items (
	task_id TEXT PRIMARY KEY,
	role TEXT NOT NULL DEFAULT '',
	priority TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT '',
	doc TEXT NOT NULL
)
