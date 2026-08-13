CREATE TABLE run_placement_decisions (
    effect_id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL UNIQUE,
    decision TEXT NOT NULL,
    worker_id TEXT,
    session_id TEXT,
    run_spec_json TEXT NOT NULL,
    command_json TEXT NOT NULL,
    created_at BIGINT NOT NULL
);
