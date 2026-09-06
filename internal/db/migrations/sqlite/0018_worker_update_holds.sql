CREATE TABLE worker_update_holds (
    worker_id TEXT PRIMARY KEY,
    hold_id TEXT NOT NULL UNIQUE,
    target_revision TEXT NOT NULL,
    previous_revision TEXT NOT NULL,
    started_at BIGINT NOT NULL
);
