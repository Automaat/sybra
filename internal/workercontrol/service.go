package workercontrol

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/executioncontract"
)

var (
	ErrStaleSession = errors.New("worker control: stale session")
	ErrLeaseExpired = errors.New("worker control: session lease expired")
	ErrEventGap     = errors.New("worker control: event sequence gap")
)

type RegisterRequest struct {
	WorkerID        string                        `json:"workerId"`
	Negotiation     executioncontract.Negotiation `json:"negotiation"`
	Capabilities    []string                      `json:"capabilities,omitempty"`
	LeaseSeconds    int                           `json:"leaseSeconds,omitempty"`
	ResumeSessionID string                        `json:"resumeSessionId,omitempty"`
	LastCommandAck  uint64                        `json:"lastCommandAck,omitempty"`
}

type Session struct {
	SessionID      string                    `json:"sessionId"`
	WorkerID       string                    `json:"workerId"`
	Version        executioncontract.Version `json:"version"`
	BuildVersion   string                    `json:"buildVersion"`
	Capabilities   []string                  `json:"capabilities,omitempty"`
	State          string                    `json:"state"`
	LeaseExpiresAt time.Time                 `json:"leaseExpiresAt"`
	LastCommandAck uint64                    `json:"lastCommandAck"`
}

type Command struct {
	Sequence uint64                            `json:"sequence"`
	Envelope executioncontract.CommandEnvelope `json:"envelope"`
}

type EventBatch struct {
	SessionID string                            `json:"sessionId"`
	Events    []executioncontract.EventEnvelope `json:"events"`
}

type ArtifactUpload struct {
	SessionID string                             `json:"sessionId"`
	Manifest  executioncontract.ArtifactManifest `json:"manifest"`
	Content   []byte                             `json:"content"`
}

type Diagnostics struct {
	WorkerID       string    `json:"workerId"`
	SessionID      string    `json:"sessionId"`
	State          string    `json:"state"`
	BuildVersion   string    `json:"buildVersion"`
	Protocol       string    `json:"protocol"`
	LeaseExpiresAt time.Time `json:"leaseExpiresAt"`
	LastCommandAck uint64    `json:"lastCommandAck"`
	ActiveRuns     int       `json:"activeRuns"`
	PendingEvents  int       `json:"pendingEvents"`
}

type Service struct {
	db       *db.DB
	now      func() time.Time
	lease    time.Duration
	notifyMu sync.Mutex
	notifyCh chan struct{}
}

func New(database *db.DB) *Service {
	return &Service{db: database, now: time.Now, lease: 45 * time.Second, notifyCh: make(chan struct{})}
}

func (s *Service) Register(ctx context.Context, request RegisterRequest) (Session, error) {
	if request.WorkerID == "" || request.Negotiation.BuildVersion == "" {
		return Session{}, errors.New("worker control: worker and build identities are required")
	}
	if request.ResumeSessionID == "" && request.LastCommandAck != 0 {
		return Session{}, errors.New("worker control: a fresh session cannot claim a command cursor")
	}
	version, err := executioncontract.Negotiate(executioncontract.Negotiation{
		ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "leader",
	}, request.Negotiation)
	if err != nil {
		return Session{}, err
	}
	lease := s.lease
	if request.LeaseSeconds > 0 {
		seconds := min(request.LeaseSeconds, int((5*time.Minute)/time.Second))
		lease = time.Duration(seconds) * time.Second
	}
	now := s.now().UTC()
	sessionID, err := randomID()
	if err != nil {
		return Session{}, fmt.Errorf("create session identity: %w", err)
	}
	capabilities, err := json.Marshal(request.Capabilities)
	if err != nil {
		return Session{}, fmt.Errorf("encode capabilities: %w", err)
	}
	state := "active"
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if request.ResumeSessionID != "" {
			query := `SELECT worker_id, state, last_command_ack, lease_expires_at FROM worker_sessions WHERE session_id = ?`
			if s.db.Dialect() == db.Postgres {
				query += ` FOR UPDATE`
			}
			var priorWorker, priorState string
			var priorAck, maxSequence uint64
			var priorExpiry int64
			if err := tx.QueryRowContext(ctx, s.db.Rebind(query), request.ResumeSessionID).Scan(&priorWorker, &priorState, &priorAck, &priorExpiry); err != nil ||
				priorWorker != request.WorkerID || (priorState != "active" && priorState != "draining") || !db.TimeFrom(priorExpiry).After(now) {
				return ErrStaleSession
			}
			state = priorState
			if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COALESCE(MAX(sequence), 0) FROM worker_commands WHERE session_id = ?`), request.ResumeSessionID).Scan(&maxSequence); err != nil {
				return err
			}
			if request.LastCommandAck < priorAck || request.LastCommandAck > max(maxSequence, priorAck) {
				return errors.New("worker control: resume command cursor is outside the durable range")
			}
		}
		fenceQuery, fenceArg := `UPDATE worker_sessions SET state = 'replaced' WHERE worker_id = ? AND state = 'active'`, request.WorkerID
		if state == "draining" {
			fenceQuery, fenceArg = `UPDATE worker_sessions SET state = 'replaced' WHERE session_id = ? AND state = 'draining'`, request.ResumeSessionID
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(fenceQuery), fenceArg); err != nil {
			return fmt.Errorf("fence prior session: %w", err)
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO worker_sessions
			(session_id, worker_id, protocol_major, protocol_minor, build_version, capabilities_json, state, lease_seconds, lease_expires_at, last_command_ack, created_at, heartbeat_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			sessionID, request.WorkerID, version.Major, version.Minor, request.Negotiation.BuildVersion, string(capabilities),
			state, int64(lease/time.Second), db.TimeValue(now.Add(lease)), request.LastCommandAck, db.TimeValue(now), db.TimeValue(now))
		if err != nil || request.ResumeSessionID == "" {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE worker_commands SET acknowledged_at = COALESCE(acknowledged_at, ?) WHERE session_id = ? AND sequence <= ?`),
			db.TimeValue(now), request.ResumeSessionID, request.LastCommandAck); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE worker_commands SET session_id = ? WHERE session_id = ? AND sequence > ?`),
			sessionID, request.ResumeSessionID, request.LastCommandAck); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, s.db.Rebind(`UPDATE remote_runs SET session_id = ?, updated_at = ? WHERE session_id = ? AND state != 'terminal'`),
			sessionID, db.TimeValue(now), request.ResumeSessionID)
		return err
	})
	if err != nil {
		return Session{}, fmt.Errorf("register worker: %w", err)
	}
	return Session{SessionID: sessionID, WorkerID: request.WorkerID, Version: version, BuildVersion: request.Negotiation.BuildVersion,
		Capabilities: request.Capabilities, State: state, LeaseExpiresAt: now.Add(lease), LastCommandAck: request.LastCommandAck}, nil
}

func (s *Service) Heartbeat(ctx context.Context, sessionID string, capabilities []string) (Session, error) {
	now := s.now().UTC()
	encoded, err := json.Marshal(capabilities)
	if err != nil {
		return Session{}, err
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		query := `SELECT state, lease_expires_at, lease_seconds FROM worker_sessions WHERE session_id = ?`
		if s.db.Dialect() == db.Postgres {
			query += ` FOR UPDATE`
		}
		var state string
		var expires, leaseSeconds int64
		if err := tx.QueryRowContext(ctx, s.db.Rebind(query), sessionID).Scan(&state, &expires, &leaseSeconds); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrStaleSession
			}
			return err
		}
		if (state != "active" && state != "draining") || !db.TimeFrom(expires).After(now) {
			return ErrStaleSession
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE worker_sessions SET heartbeat_at = ?, lease_expires_at = ?, capabilities_json = ? WHERE session_id = ?`),
			db.TimeValue(now), db.TimeValue(now.Add(time.Duration(leaseSeconds)*time.Second)), string(encoded), sessionID)
		return err
	})
	if err != nil {
		return Session{}, err
	}
	return s.session(ctx, sessionID)
}

func (s *Service) Enqueue(ctx context.Context, sessionID string, spec *executioncontract.RunSpec, envelope executioncontract.CommandEnvelope) (Command, error) {
	if err := envelope.Validate(); err != nil {
		return Command{}, err
	}
	if err := validateStartDelivery(spec, envelope); err != nil {
		return Command{}, err
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return Command{}, err
	}
	var sequence uint64
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := s.requireSessionTx(ctx, tx, sessionID, false); err != nil {
			return err
		}
		var existing uint64
		var existingPayload, existingSession string
		var acknowledgedAt sql.NullInt64
		err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT sequence, payload_json, session_id, acknowledged_at FROM worker_commands WHERE idempotency_key = ?`), envelope.IdempotencyKey).
			Scan(&existing, &existingPayload, &existingSession, &acknowledgedAt)
		if err == nil {
			if existingPayload != string(payload) {
				return errors.New("worker control: idempotency key reused for a different command")
			}
			if existingSession != sessionID {
				if err := s.validateCrossSessionReplay(ctx, tx, sessionID, existing, acknowledgedAt.Valid, envelope); err != nil {
					return err
				}
			}
			sequence = existing
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if envelope.Type != executioncontract.CommandStart {
			var ownerSession string
			if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT session_id FROM remote_runs WHERE run_id = ?`), envelope.RunID).Scan(&ownerSession); err != nil {
				return fmt.Errorf("worker control: command target: %w", err)
			}
			if ownerSession != sessionID {
				return ErrStaleSession
			}
		}
		if spec != nil && envelope.Type == executioncontract.CommandStart {
			specJSON, _ := json.Marshal(spec)
			var priorRun, priorSpec string
			err = tx.QueryRowContext(ctx, s.db.Rebind(`SELECT run_id, run_spec_json FROM remote_runs WHERE effect_id = ?`), spec.EffectID).
				Scan(&priorRun, &priorSpec)
			switch {
			case err == nil:
				if priorRun != spec.RunID || priorSpec != string(specJSON) {
					return errors.New("worker control: effect id already belongs to another fenced run")
				}
				var startSession string
				var startAcknowledged sql.NullInt64
				if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT sequence, session_id, acknowledged_at FROM worker_commands WHERE run_id = ? AND command_type = ? ORDER BY sequence LIMIT 1`),
					spec.RunID, executioncontract.CommandStart).Scan(&sequence, &startSession, &startAcknowledged); err != nil {
					return err
				}
				if startSession != sessionID {
					if err := s.validateCrossSessionReplay(ctx, tx, sessionID, sequence, startAcknowledged.Valid, envelope); err != nil {
						return err
					}
				}
				return nil
			case errors.Is(err, sql.ErrNoRows):
				_, err = tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO remote_runs
					(run_id, worker_id, session_id, effect_id, task_id, task_generation, workflow_id, workflow_generation, step_id, run_spec_json, state, updated_at)
					SELECT ?, worker_id, session_id, ?, ?, ?, ?, ?, ?, ?, 'queued', ? FROM worker_sessions WHERE session_id = ?`),
					spec.RunID, spec.EffectID, spec.Fence.TaskID, spec.Fence.TaskGeneration, spec.Fence.WorkflowID,
					spec.Fence.WorkflowGeneration, spec.Fence.StepID, string(specJSON), db.TimeValue(s.now().UTC()), sessionID)
				if err != nil {
					return err
				}
			default:
				return err
			}
		}
		var lastAck, maxSequence uint64
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT last_command_ack FROM worker_sessions WHERE session_id = ?`), sessionID).Scan(&lastAck); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COALESCE(MAX(sequence), 0) FROM worker_commands WHERE session_id = ?`), sessionID).Scan(&maxSequence); err != nil {
			return err
		}
		sequence = max(lastAck, maxSequence) + 1
		_, err = tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO worker_commands
            (session_id, sequence, command_id, run_id, idempotency_key, command_type, payload_json, created_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?)`), sessionID, sequence, envelope.CommandID, envelope.RunID,
			envelope.IdempotencyKey, envelope.Type, string(payload), db.TimeValue(s.now().UTC()))
		return err
	})
	if err != nil {
		return Command{}, err
	}
	s.notify()
	return Command{Sequence: sequence, Envelope: envelope}, nil
}

func (s *Service) validateCrossSessionReplay(ctx context.Context, tx *sql.Tx, sessionID string, sequence uint64, acknowledged bool, envelope executioncontract.CommandEnvelope) error {
	if envelope.Type != executioncontract.CommandStart || !acknowledged {
		return ErrStaleSession
	}
	var owner string
	var inheritedAck uint64
	err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT r.session_id, s.last_command_ack FROM remote_runs r JOIN worker_sessions s ON s.session_id = r.session_id WHERE r.run_id = ?`), envelope.RunID).
		Scan(&owner, &inheritedAck)
	if err != nil || owner != sessionID || inheritedAck < sequence {
		return ErrStaleSession
	}
	return nil
}

func validateStartDelivery(spec *executioncontract.RunSpec, envelope executioncontract.CommandEnvelope) error {
	if spec == nil {
		if envelope.Type == executioncontract.CommandStart {
			return errors.New("worker control: start command requires a durable run spec")
		}
		return nil
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	if envelope.Type != executioncontract.CommandStart {
		return errors.New("worker control: run spec is only valid for a start command")
	}
	if spec.RunID != envelope.RunID {
		return errors.New("worker control: command and run spec identities differ")
	}
	var start executioncontract.StartCommandPayload
	if err := json.Unmarshal(envelope.Payload, &start); err != nil || start.Spec == nil {
		return errors.New("worker control: durable start delivery requires an inline run spec")
	}
	provided, _ := json.Marshal(spec)
	embedded, _ := json.Marshal(start.Spec)
	if !bytes.Equal(provided, embedded) {
		return errors.New("worker control: command payload and run spec differ")
	}
	return nil
}

func (s *Service) PollCommands(ctx context.Context, sessionID string, after uint64, limit int, wait time.Duration) ([]Command, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	deadline := time.NewTimer(min(max(wait, 0), 25*time.Second))
	defer deadline.Stop()
	for {
		notified := s.notification()
		commands, err := s.commands(ctx, sessionID, after, limit)
		if err != nil || len(commands) > 0 || wait <= 0 {
			return commands, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return []Command{}, nil
		case <-notified:
		}
	}
}

func (s *Service) AckCommands(ctx context.Context, sessionID string, through uint64) error {
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := s.requireSessionTx(ctx, tx, sessionID, true); err != nil {
			return err
		}
		var maxSequence, lastAck uint64
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COALESCE(MAX(sequence), 0) FROM worker_commands WHERE session_id = ?`), sessionID).Scan(&maxSequence); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT last_command_ack FROM worker_sessions WHERE session_id = ?`), sessionID).Scan(&lastAck); err != nil {
			return err
		}
		if through > max(maxSequence, lastAck) {
			return errors.New("worker control: command acknowledgement exceeds delivered cursor")
		}
		now := db.TimeValue(s.now().UTC())
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE worker_commands SET acknowledged_at = COALESCE(acknowledged_at, ?) WHERE session_id = ? AND sequence <= ?`), now, sessionID, through); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE worker_sessions SET last_command_ack = CASE WHEN last_command_ack < ? THEN ? ELSE last_command_ack END WHERE session_id = ?`), through, through, sessionID)
		return err
	})
}

func (s *Service) AppendEvents(ctx context.Context, batch EventBatch) (map[string]uint64, error) {
	acks := map[string]uint64{}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := s.requireSessionTx(ctx, tx, batch.SessionID, true); err != nil {
			return err
		}
		for i := range batch.Events {
			event := batch.Events[i]
			if err := event.Validate(); err != nil {
				return err
			}
			var current uint64
			var ownerSession, state string
			if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT last_event_sequence, session_id, state FROM remote_runs WHERE run_id = ?`), event.RunID).Scan(&current, &ownerSession, &state); err != nil {
				return err
			}
			if ownerSession != batch.SessionID {
				return ErrStaleSession
			}
			if event.Sequence <= current {
				var stored string
				if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT envelope_json FROM worker_events WHERE run_id = ? AND sequence = ?`), event.RunID, event.Sequence).Scan(&stored); err != nil {
					return err
				}
				encoded, _ := json.Marshal(event)
				if stored != string(encoded) {
					return errors.New("worker control: replayed event differs from durable event")
				}
				acks[event.RunID] = current
				continue
			}
			if event.Sequence != current+1 {
				return ErrEventGap
			}
			if state == "terminal" {
				return errors.New("worker control: event follows terminal event")
			}
			if current > 0 {
				var previousJSON string
				if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT envelope_json FROM worker_events WHERE run_id = ? AND sequence = ?`), event.RunID, current).Scan(&previousJSON); err != nil {
					return err
				}
				var previous executioncontract.EventEnvelope
				if err := json.Unmarshal([]byte(previousJSON), &previous); err != nil {
					return err
				}
				if previous.Version != event.Version || previous.BuildVersion != event.BuildVersion {
					return errors.New("worker control: event stream mixes build or protocol identities")
				}
			}
			encoded, err := json.Marshal(event)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO worker_events
                (run_id, sequence, session_id, event_id, idempotency_key, event_type, envelope_json, observed_at)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?)`), event.RunID, event.Sequence, batch.SessionID, event.EventID,
				event.IdempotencyKey, event.Type, string(encoded), db.TimeValue(event.ObservedAt))
			if err != nil {
				return err
			}
			nextState := "running"
			if event.Type == executioncontract.EventTerminal {
				nextState = "terminal"
			}
			_, err = tx.ExecContext(ctx, s.db.Rebind(`UPDATE remote_runs SET last_event_sequence = ?, state = ?, updated_at = ? WHERE run_id = ?`),
				event.Sequence, nextState, db.TimeValue(s.now().UTC()), event.RunID)
			if err != nil {
				return err
			}
			acks[event.RunID] = event.Sequence
		}
		return nil
	})
	return acks, err
}

func (s *Service) ReplayEvents(ctx context.Context, runID string, after uint64, limit int) ([]executioncontract.EventEnvelope, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT envelope_json FROM worker_events WHERE run_id = ? AND sequence > ? ORDER BY sequence LIMIT ?`, runID, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	events := []executioncontract.EventEnvelope{}
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return nil, err
		}
		var event executioncontract.EventEnvelope
		if err := json.Unmarshal([]byte(encoded), &event); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Service) AckEvents(ctx context.Context, sessionID, runID string, through uint64) error {
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := s.requireSessionTx(ctx, tx, sessionID, true); err != nil {
			return err
		}
		var delivered uint64
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT last_event_sequence FROM remote_runs WHERE run_id = ? AND session_id = ?`), runID, sessionID).Scan(&delivered); err != nil {
			return err
		}
		if through > delivered {
			return errors.New("worker control: event acknowledgement exceeds durable cursor")
		}
		now := db.TimeValue(s.now().UTC())
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE worker_events SET acknowledged_at = COALESCE(acknowledged_at, ?) WHERE run_id = ? AND sequence <= ?`), now, runID, through); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE remote_runs SET last_event_ack = CASE WHEN last_event_ack < ? THEN ? ELSE last_event_ack END WHERE run_id = ?`), through, through, runID)
		return err
	})
}

func (s *Service) UploadArtifact(ctx context.Context, upload ArtifactUpload) error {
	if err := upload.Manifest.Validate(); err != nil {
		return err
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := s.requireSessionTx(ctx, tx, upload.SessionID, true); err != nil {
			return err
		}
		var runSession string
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT session_id FROM remote_runs WHERE run_id = ?`), upload.Manifest.RunID).Scan(&runSession); err != nil {
			return err
		}
		if runSession != upload.SessionID {
			return ErrStaleSession
		}
		encoded, _ := json.Marshal(upload.Manifest)
		var existingManifest, existingRun string
		var existingContent []byte
		err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT manifest_json, run_id, content FROM worker_artifacts WHERE idempotency_key = ?`),
			upload.Manifest.IdempotencyKey).Scan(&existingManifest, &existingRun, &existingContent)
		if err == nil {
			if existingManifest != string(encoded) || existingRun != upload.Manifest.RunID || !bytes.Equal(existingContent, upload.Content) {
				return errors.New("worker control: artifact idempotency key reused for different content")
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO worker_artifacts
            (manifest_id, run_id, session_id, idempotency_key, manifest_json, content, imported_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`), upload.Manifest.ManifestID,
			upload.Manifest.RunID, upload.SessionID, upload.Manifest.IdempotencyKey, string(encoded), upload.Content, db.TimeValue(s.now().UTC()))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, s.db.Rebind(`UPDATE remote_runs SET artifact_state = ?, updated_at = ? WHERE run_id = ?`),
			upload.Manifest.State, db.TimeValue(s.now().UTC()), upload.Manifest.RunID)
		return err
	})
}

func (s *Service) Drain(ctx context.Context, sessionID string) error {
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := s.requireSessionTx(ctx, tx, sessionID, false); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE worker_sessions SET state = 'draining' WHERE session_id = ? AND state = 'active'`), sessionID)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return ErrStaleSession
		}
		return nil
	})
}

func (s *Service) Diagnostics(ctx context.Context) ([]Diagnostics, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.worker_id, s.session_id, s.state, s.build_version, s.protocol_major,
        s.protocol_minor, s.lease_expires_at, s.last_command_ack,
        (SELECT COUNT(*) FROM remote_runs r WHERE r.session_id = s.session_id AND r.state != 'terminal'),
        (SELECT COUNT(*) FROM worker_events e JOIN remote_runs r ON r.run_id = e.run_id WHERE r.session_id = s.session_id AND e.acknowledged_at IS NULL)
        FROM worker_sessions s ORDER BY s.worker_id, s.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Diagnostics{}
	for rows.Next() {
		var item Diagnostics
		var major, minor int
		var lease int64
		if err := rows.Scan(&item.WorkerID, &item.SessionID, &item.State, &item.BuildVersion, &major, &minor, &lease,
			&item.LastCommandAck, &item.ActiveRuns, &item.PendingEvents); err != nil {
			return nil, err
		}
		item.Protocol = fmt.Sprintf("%d.%d", major, minor)
		item.LeaseExpiresAt = db.TimeFrom(lease)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Service) commands(ctx context.Context, sessionID string, after uint64, limit int) ([]Command, error) {
	if _, err := s.session(ctx, sessionID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sequence, payload_json FROM worker_commands WHERE session_id = ? AND sequence > ? ORDER BY sequence LIMIT ?`, sessionID, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Command{}
	for rows.Next() {
		var command Command
		var payload string
		if err := rows.Scan(&command.Sequence, &payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(payload), &command.Envelope); err != nil {
			return nil, err
		}
		out = append(out, command)
	}
	return out, rows.Err()
}

func (s *Service) session(ctx context.Context, sessionID string) (Session, error) {
	var result Session
	var capabilities string
	var major, minor int
	var lease int64
	err := s.db.QueryRowContext(ctx, `SELECT worker_id, protocol_major, protocol_minor, build_version, capabilities_json,
        state, lease_expires_at, last_command_ack FROM worker_sessions WHERE session_id = ?`, sessionID).
		Scan(&result.WorkerID, &major, &minor, &result.BuildVersion, &capabilities, &result.State, &lease, &result.LastCommandAck)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, ErrStaleSession
		}
		return Session{}, err
	}
	result.SessionID, result.Version, result.LeaseExpiresAt = sessionID, executioncontract.Version{Major: major, Minor: minor}, db.TimeFrom(lease)
	_ = json.Unmarshal([]byte(capabilities), &result.Capabilities)
	if result.State != "active" && result.State != "draining" {
		return Session{}, ErrStaleSession
	}
	if !result.LeaseExpiresAt.After(s.now().UTC()) {
		return Session{}, ErrLeaseExpired
	}
	return result, nil
}

func (s *Service) requireSessionTx(ctx context.Context, tx *sql.Tx, sessionID string, allowDraining bool) error {
	query := `SELECT state, lease_expires_at FROM worker_sessions WHERE session_id = ?`
	if s.db.Dialect() == db.Postgres {
		query += ` FOR UPDATE`
	}
	var state string
	var lease int64
	if err := tx.QueryRowContext(ctx, s.db.Rebind(query), sessionID).Scan(&state, &lease); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrStaleSession
		}
		return err
	}
	if state != "active" && (!allowDraining || state != "draining") {
		return ErrStaleSession
	}
	if !db.TimeFrom(lease).After(s.now().UTC()) {
		return ErrLeaseExpired
	}
	return nil
}

func (s *Service) notify() {
	s.notifyMu.Lock()
	close(s.notifyCh)
	s.notifyCh = make(chan struct{})
	s.notifyMu.Unlock()
}

func (s *Service) notification() <-chan struct{} {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	return s.notifyCh
}

func randomID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "session-" + hex.EncodeToString(value[:]), nil
}
