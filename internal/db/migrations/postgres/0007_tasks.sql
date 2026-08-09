CREATE TABLE IF NOT EXISTS tasks (
	id TEXT PRIMARY KEY,
	status TEXT NOT NULL DEFAULT '',
	project_id TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL DEFAULT '',
	created_at BIGINT NOT NULL DEFAULT 0,
	updated_at BIGINT NOT NULL DEFAULT 0,
	deleted_at BIGINT NOT NULL DEFAULT 0,
	doc TEXT NOT NULL
)
--;;
CREATE INDEX IF NOT EXISTS tasks_status_idx ON tasks (status, updated_at)
--;;
CREATE INDEX IF NOT EXISTS tasks_project_idx ON tasks (project_id, updated_at)
--;;
CREATE INDEX IF NOT EXISTS tasks_deleted_idx ON tasks (deleted_at)
--;;
CREATE TABLE IF NOT EXISTS task_sidecars (
	task_id TEXT NOT NULL,
	kind TEXT NOT NULL,
	name TEXT NOT NULL DEFAULT '',
	updated_at BIGINT NOT NULL DEFAULT 0,
	content TEXT NOT NULL,
	PRIMARY KEY (task_id, kind, name)
)
--;;
CREATE INDEX IF NOT EXISTS task_sidecars_task_idx ON task_sidecars (task_id, kind)
