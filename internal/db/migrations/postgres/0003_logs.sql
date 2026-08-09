ALTER TABLE schema_imports ADD COLUMN cursor TEXT NOT NULL DEFAULT ''
--;;
ALTER TABLE schema_imports ADD COLUMN done SMALLINT NOT NULL DEFAULT 1
--;;
CREATE TABLE IF NOT EXISTS audit_events (
	id TEXT PRIMARY KEY,
	ts BIGINT NOT NULL,
	event_type TEXT NOT NULL DEFAULT '',
	task_id TEXT NOT NULL DEFAULT '',
	agent_id TEXT NOT NULL DEFAULT '',
	doc TEXT NOT NULL
)
--;;
CREATE INDEX IF NOT EXISTS audit_events_ts_idx ON audit_events (ts)
--;;
CREATE INDEX IF NOT EXISTS audit_events_task_idx ON audit_events (task_id, ts)
--;;
CREATE TABLE IF NOT EXISTS tool_ledger (
	id TEXT PRIMARY KEY,
	ts BIGINT NOT NULL,
	agent_id TEXT NOT NULL DEFAULT '',
	task_id TEXT NOT NULL DEFAULT '',
	tool TEXT NOT NULL DEFAULT '',
	doc TEXT NOT NULL
)
--;;
CREATE INDEX IF NOT EXISTS tool_ledger_ts_idx ON tool_ledger (ts)
--;;
CREATE INDEX IF NOT EXISTS tool_ledger_task_idx ON tool_ledger (task_id, ts)
--;;
CREATE TABLE IF NOT EXISTS run_records (
	id TEXT PRIMARY KEY,
	task_id TEXT NOT NULL DEFAULT '',
	project_id TEXT NOT NULL DEFAULT '',
	started_at BIGINT NOT NULL DEFAULT 0,
	doc TEXT NOT NULL
)
--;;
CREATE INDEX IF NOT EXISTS run_records_started_idx ON run_records (started_at)
--;;
CREATE INDEX IF NOT EXISTS run_records_task_idx ON run_records (task_id, started_at)
--;;
CREATE TABLE IF NOT EXISTS provider_quota_snapshots (
	provider TEXT PRIMARY KEY,
	captured_at BIGINT NOT NULL DEFAULT 0,
	doc TEXT NOT NULL
)
--;;
CREATE TABLE IF NOT EXISTS provider_quota_usage (
	id TEXT PRIMARY KEY,
	provider TEXT NOT NULL DEFAULT '',
	ts BIGINT NOT NULL,
	doc TEXT NOT NULL
)
--;;
CREATE INDEX IF NOT EXISTS provider_quota_usage_idx ON provider_quota_usage (provider, ts)
