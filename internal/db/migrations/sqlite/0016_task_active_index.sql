CREATE INDEX IF NOT EXISTS tasks_active_idx ON tasks (deleted_at, status, id)
