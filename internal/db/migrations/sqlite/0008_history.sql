CREATE TABLE IF NOT EXISTS task_history (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id TEXT NOT NULL,
	actor TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL DEFAULT '',
	changed_at INTEGER NOT NULL DEFAULT 0,
	fields TEXT NOT NULL DEFAULT '',
	snapshot TEXT NOT NULL
)
--;;
CREATE INDEX IF NOT EXISTS task_history_task_idx ON task_history (task_id, changed_at)
--;;
CREATE INDEX IF NOT EXISTS task_history_actor_idx ON task_history (actor, changed_at)
--;;
CREATE INDEX IF NOT EXISTS task_history_time_idx ON task_history (changed_at)
