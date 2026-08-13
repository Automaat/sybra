ALTER TABLE worker_sessions ADD COLUMN registration_id TEXT;
--;;
CREATE UNIQUE INDEX worker_sessions_registration_id ON worker_sessions(registration_id) WHERE registration_id IS NOT NULL;
