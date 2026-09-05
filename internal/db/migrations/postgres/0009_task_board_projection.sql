ALTER TABLE tasks ADD COLUMN board_doc TEXT NOT NULL DEFAULT ''
--;;
ALTER TABLE tasks ADD COLUMN assigned_node TEXT NOT NULL DEFAULT ''
--;;
ALTER TABLE tasks ADD COLUMN closed_at BIGINT NOT NULL DEFAULT 0
--;;
CREATE INDEX IF NOT EXISTS tasks_node_idx ON tasks (assigned_node, status, closed_at, deleted_at)
