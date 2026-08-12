CREATE TABLE worker_sessions (
    session_id TEXT PRIMARY KEY,
    worker_id TEXT NOT NULL,
    protocol_major INTEGER NOT NULL,
    protocol_minor INTEGER NOT NULL,
    build_version TEXT NOT NULL,
    capabilities_json TEXT NOT NULL,
    state TEXT NOT NULL,
    lease_seconds BIGINT NOT NULL,
    lease_expires_at BIGINT NOT NULL,
    last_command_ack BIGINT NOT NULL DEFAULT 0,
    created_at BIGINT NOT NULL,
    heartbeat_at BIGINT NOT NULL
);
--;;
CREATE UNIQUE INDEX worker_sessions_one_active ON worker_sessions(worker_id) WHERE state = 'active';
--;;
CREATE TABLE remote_runs (
    run_id TEXT PRIMARY KEY,
    worker_id TEXT NOT NULL,
    session_id TEXT NOT NULL REFERENCES worker_sessions(session_id),
    effect_id TEXT NOT NULL UNIQUE,
    task_id TEXT NOT NULL,
    task_generation BIGINT NOT NULL,
    workflow_id TEXT NOT NULL,
    workflow_generation BIGINT NOT NULL,
    step_id TEXT NOT NULL,
    run_spec_json TEXT NOT NULL,
    state TEXT NOT NULL,
    last_event_sequence BIGINT NOT NULL DEFAULT 0,
    last_event_ack BIGINT NOT NULL DEFAULT 0,
    artifact_state TEXT NOT NULL DEFAULT 'pending',
    updated_at BIGINT NOT NULL
);
--;;
CREATE TABLE worker_commands (
    session_id TEXT NOT NULL REFERENCES worker_sessions(session_id),
    sequence BIGINT NOT NULL,
    command_id TEXT NOT NULL UNIQUE,
    run_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL UNIQUE,
    command_type TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    created_at BIGINT NOT NULL,
    acknowledged_at BIGINT,
    PRIMARY KEY(session_id, sequence)
);
--;;
CREATE TABLE worker_events (
    run_id TEXT NOT NULL REFERENCES remote_runs(run_id),
    sequence BIGINT NOT NULL,
    session_id TEXT NOT NULL,
    event_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    event_type TEXT NOT NULL,
    envelope_json TEXT NOT NULL,
    observed_at BIGINT NOT NULL,
    acknowledged_at BIGINT,
    PRIMARY KEY(run_id, sequence),
    UNIQUE(run_id, event_id),
    UNIQUE(run_id, idempotency_key)
);
--;;
CREATE TABLE worker_artifacts (
    manifest_id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES remote_runs(run_id),
    session_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL UNIQUE,
    manifest_json TEXT NOT NULL,
    content BLOB NOT NULL,
    imported_at BIGINT NOT NULL
);
