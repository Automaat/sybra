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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/agentgrant"
	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/executioncontract"
)

var (
	ErrStaleSession   = errors.New("worker control: stale session")
	ErrLeaseExpired   = errors.New("worker control: session lease expired")
	ErrEventGap       = errors.New("worker control: event sequence gap")
	ErrInvalidRequest = errors.New("worker control: invalid request")
)

type RegisterRequest struct {
	WorkerID        string                        `json:"workerId"`
	RegistrationID  string                        `json:"registrationId,omitempty"`
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
	SessionID      string                            `json:"sessionId"`
	Events         []executioncontract.EventEnvelope `json:"events"`
	Authorizations map[string]RunActionRequest       `json:"authorizations,omitempty"`
}

type ArtifactUpload struct {
	SessionID string                             `json:"sessionId"`
	Manifest  executioncontract.ArtifactManifest `json:"manifest"`
	Content   []byte                             `json:"content"`
}

type ArtifactHandback struct {
	Spec     executioncontract.RunSpec
	Manifest executioncontract.ArtifactManifest
	Package  executioncontract.ArtifactPackage
	Content  []byte
	State    string
}

type Diagnostics struct {
	WorkerID          string    `json:"workerId"`
	SessionID         string    `json:"sessionId"`
	State             string    `json:"state"`
	BuildVersion      string    `json:"buildVersion"`
	Protocol          string    `json:"protocol"`
	LeaseExpiresAt    time.Time `json:"leaseExpiresAt"`
	LastCommandAck    uint64    `json:"lastCommandAck"`
	ActiveRuns        int       `json:"activeRuns"`
	PendingEvents     int       `json:"pendingEvents"`
	Capacity          int       `json:"capacity"`
	AvailableCapacity int       `json:"availableCapacity"`
	SpoolBytes        int64     `json:"spoolBytes"`
	SpoolMaxBytes     int64     `json:"spoolMaxBytes"`
	Alerts            []string  `json:"alerts,omitempty"`
}

type RemoteRunStatus struct {
	RunID         string
	SessionID     string
	State         string
	ArtifactState string
}

type RemoteRun struct {
	Spec   executioncontract.RunSpec
	Status RemoteRunStatus
}

type Service struct {
	db             *db.DB
	now            func() time.Time
	lease          time.Duration
	notifyMu       sync.Mutex
	notifyCh       chan struct{}
	importArtifact func(context.Context, string) error
	grants         *agentgrant.Store
}

func New(database *db.DB) *Service {
	grants, _ := agentgrant.New("", 15*time.Minute)
	return NewWithGrantStore(database, grants)
}

func NewWithGrantStore(database *db.DB, grants *agentgrant.Store) *Service {
	return &Service{db: database, now: time.Now, lease: 45 * time.Second, notifyCh: make(chan struct{}), grants: grants}
}

func (s *Service) RemoteRunStatus(ctx context.Context, runID string) (RemoteRunStatus, error) {
	var status RemoteRunStatus
	status.RunID = runID
	err := s.db.QueryRowContext(ctx, s.db.Rebind(`SELECT session_id, state, artifact_state FROM remote_runs WHERE run_id = ?`), runID).
		Scan(&status.SessionID, &status.State, &status.ArtifactState)
	return status, err
}

// RemoteRunForEffect returns the immutable run already fenced to effectID.
// Leader recovery uses it to reattach durable event delivery after a restart;
// it never reserves capacity or enqueues replacement provider work.
func (s *Service) RemoteRunForEffect(ctx context.Context, effectID string) (RemoteRun, error) {
	var run RemoteRun
	var encoded string
	err := s.db.QueryRowContext(ctx, s.db.Rebind(`SELECT run_id, session_id, state, artifact_state, run_spec_json FROM remote_runs WHERE effect_id = ?`), effectID).
		Scan(&run.Status.RunID, &run.Status.SessionID, &run.Status.State, &run.Status.ArtifactState, &encoded)
	if err != nil {
		return RemoteRun{}, err
	}
	if err := json.Unmarshal([]byte(encoded), &run.Spec); err != nil {
		return RemoteRun{}, fmt.Errorf("worker control: decode fenced remote run: %w", err)
	}
	if err := run.Spec.Validate(); err != nil {
		return RemoteRun{}, fmt.Errorf("worker control: invalid fenced remote run: %w", err)
	}
	return run, nil
}

type RunGrant struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type RunActionRequest struct {
	SessionID          string `json:"sessionId"`
	Token              string `json:"token"`
	TaskID             string `json:"taskId"`
	EffectID           string `json:"effectId"`
	WorkflowGeneration int64  `json:"workflowGeneration"`
	Action             string `json:"action"`
	ReplayKey          string `json:"replayKey"`
}

func (s *Service) authorizeRunActionTx(ctx context.Context, tx *sql.Tx, runID string, request RunActionRequest) error {
	query := `SELECT task_id, effect_id, workflow_generation FROM remote_runs WHERE run_id = ? AND session_id = ? AND state IN ('queued', 'running')`
	if s.db.Dialect() == db.Postgres {
		query += ` FOR UPDATE`
	}
	var taskID, effectID string
	var generation int64
	if err := tx.QueryRowContext(ctx, s.db.Rebind(query), runID, request.SessionID).Scan(&taskID, &effectID, &generation); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrStaleSession
		}
		return err
	}
	if taskID != request.TaskID || effectID != request.EffectID || generation != request.WorkflowGeneration {
		return ErrInvalidRequest
	}
	use := agentgrant.Use{
		TaskID: taskID, RunID: runID, EffectID: effectID, WorkflowGeneration: generation,
		Action: request.Action, ReplayKey: request.ReplayKey,
	}
	return s.grants.Check(request.Token, use)
}

func (s *Service) IssueRunGrant(ctx context.Context, sessionID, runID string) (RunGrant, error) {
	if strings.TrimSpace(runID) == "" {
		return RunGrant{}, invalidf("run is required")
	}
	var token string
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := s.requireSessionTx(ctx, tx, sessionID, true); err != nil {
			return err
		}
		query := `SELECT run_spec_json FROM remote_runs WHERE run_id = ? AND session_id = ? AND state IN ('queued', 'running')`
		if s.db.Dialect() == db.Postgres {
			query += ` FOR UPDATE`
		}
		var specJSON string
		if err := tx.QueryRowContext(ctx, s.db.Rebind(query), runID, sessionID).Scan(&specJSON); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrStaleSession
			}
			return err
		}
		var spec executioncontract.RunSpec
		if err := json.Unmarshal([]byte(specJSON), &spec); err != nil {
			return err
		}
		var mintErr error
		token, mintErr = s.grants.MintScoped(agentgrant.Grant{
			TaskID: spec.Fence.TaskID, RunID: spec.RunID, EffectID: spec.EffectID, WorkflowGeneration: spec.Fence.WorkflowGeneration,
			AllowedActions: []string{"approval.request"},
		})
		return mintErr
	})
	if err != nil {
		if revokeErr := s.grants.RevokeToken(token); revokeErr != nil {
			return RunGrant{}, errors.Join(err, fmt.Errorf("revoke uncommitted grant: %w", revokeErr))
		}
		return RunGrant{}, err
	}
	grant, _ := s.grants.Verify(token)
	return RunGrant{Token: token, ExpiresAt: grant.ExpiresAt}, nil
}

// SetArtifactImporter late-binds the leader-owned canonical importer after
// task/worktree stores are initialized.
func (s *Service) SetArtifactImporter(importer func(context.Context, string) error) {
	s.importArtifact = importer
}

func (s *Service) SetGrantAuditSink(sink agentgrant.AuditSink) {
	s.grants.SetAuditSink(sink)
}

func (s *Service) Register(ctx context.Context, request RegisterRequest) (Session, error) {
	if request.WorkerID == "" || request.Negotiation.BuildVersion == "" {
		return Session{}, invalidf("worker and build identities are required")
	}
	if request.ResumeSessionID == "" && request.LastCommandAck != 0 {
		return Session{}, invalidf("a fresh session cannot claim a command cursor")
	}
	if request.RegistrationID != "" && !validRegistrationID(request.RegistrationID) {
		return Session{}, invalidf("registration identity is malformed")
	}
	version, err := executioncontract.Negotiate(executioncontract.Negotiation{
		ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "leader",
	}, request.Negotiation)
	if err != nil {
		return Session{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
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
	leaseExpiresAt := now.Add(lease)
	lastCommandAck := request.LastCommandAck
	responseCapabilities := append([]string(nil), request.Capabilities...)
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if request.RegistrationID != "" {
			var existingWorker, existingBuild, existingCapabilities, existingState string
			var existingMajor, existingMinor, existingLease, existingExpiry int64
			var existingAck uint64
			err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT session_id, worker_id, protocol_major, protocol_minor, build_version,
				capabilities_json, state, lease_seconds, lease_expires_at, last_command_ack
				FROM worker_sessions WHERE registration_id = ?`), request.RegistrationID).
				Scan(&sessionID, &existingWorker, &existingMajor, &existingMinor, &existingBuild, &existingCapabilities,
					&existingState, &existingLease, &existingExpiry, &existingAck)
			if err == nil {
				if existingWorker != request.WorkerID || existingMajor != int64(version.Major) || existingMinor != int64(version.Minor) ||
					existingBuild != request.Negotiation.BuildVersion || existingLease != int64(lease/time.Second) || existingAck != request.LastCommandAck {
					return invalidf("registration identity was reused for a different request")
				}
				if existingState != "active" && existingState != "draining" && existingState != "disabled" {
					return ErrStaleSession
				}
				if err := json.Unmarshal([]byte(existingCapabilities), &responseCapabilities); err != nil {
					return fmt.Errorf("decode replayed worker capabilities: %w", err)
				}
				// Replaying a registration proves the same daemon still owns the
				// durable operation. Renew the committed successor so a lost HTTP
				// response that outlives one lease cannot trap retries on an expired
				// session forever.
				leaseExpiresAt = now.Add(lease)
				if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE worker_sessions SET lease_expires_at = ?, heartbeat_at = ? WHERE session_id = ?`),
					db.TimeValue(leaseExpiresAt), db.TimeValue(now), sessionID); err != nil {
					return err
				}
				state, lastCommandAck = existingState, existingAck
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		var disabled int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM worker_disabled WHERE worker_id = ?`), request.WorkerID).Scan(&disabled); err != nil {
			return err
		}
		if disabled > 0 {
			state = "disabled"
		}
		if request.ResumeSessionID != "" {
			query := `SELECT worker_id, state, last_command_ack FROM worker_sessions WHERE session_id = ?`
			if s.db.Dialect() == db.Postgres {
				query += ` FOR UPDATE`
			}
			var priorWorker, priorState string
			var priorAck, maxSequence uint64
			if err := tx.QueryRowContext(ctx, s.db.Rebind(query), request.ResumeSessionID).Scan(&priorWorker, &priorState, &priorAck); err != nil ||
				priorWorker != request.WorkerID || (priorState != "active" && priorState != "draining" && priorState != "disabled") {
				return ErrStaleSession
			}
			// Resume is also the recovery proof after a lease expires. The opaque
			// session ID still has to name this worker's current, unreplaced
			// session; accepting it here lets a disconnected daemon migrate its
			// durable commands and live runs without falsely starting them again.
			// Ordinary control/event calls continue to reject expired leases.
			if disabled == 0 {
				state = priorState
			}
			if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COALESCE(MAX(sequence), 0) FROM worker_commands WHERE session_id = ?`), request.ResumeSessionID).Scan(&maxSequence); err != nil {
				return err
			}
			if request.LastCommandAck < priorAck || request.LastCommandAck > max(maxSequence, priorAck) {
				return invalidf("resume command cursor is outside the durable range")
			}
		}
		fenceQuery, fenceArg := `UPDATE worker_sessions SET state = 'replaced' WHERE worker_id = ? AND state = 'active'`, request.WorkerID
		if request.ResumeSessionID == "" {
			fenceQuery = `UPDATE worker_sessions SET state = 'replaced' WHERE worker_id = ? AND state IN ('active', 'draining', 'disabled')`
		} else if state == "draining" || state == "disabled" {
			fenceQuery, fenceArg = `UPDATE worker_sessions SET state = 'replaced' WHERE session_id = ? AND state IN ('draining', 'disabled')`, request.ResumeSessionID
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(fenceQuery), fenceArg); err != nil {
			return fmt.Errorf("fence prior session: %w", err)
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO worker_sessions
			(session_id, worker_id, registration_id, protocol_major, protocol_minor, build_version, capabilities_json, state, lease_seconds, lease_expires_at, last_command_ack, created_at, heartbeat_at)
			VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			sessionID, request.WorkerID, request.RegistrationID, version.Major, version.Minor, request.Negotiation.BuildVersion, string(capabilities),
			state, int64(lease/time.Second), db.TimeValue(now.Add(lease)), request.LastCommandAck, db.TimeValue(now), db.TimeValue(now))
		if err != nil {
			return err
		}
		if request.ResumeSessionID == "" {
			// A daemon with no live provider may fall back to a fresh session after
			// its lease expires. Completed handbacks still belong to the stable
			// worker identity and must remain uploadable; nonterminal execution is
			// deliberately not adopted by this path.
			_, err = tx.ExecContext(ctx, s.db.Rebind(`UPDATE remote_runs SET session_id = ?, updated_at = ? WHERE worker_id = ? AND state = 'terminal' AND artifact_state IN ('pending', 'staged', 'importing')`),
				sessionID, db.TimeValue(now), request.WorkerID)
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
		// Exact resume transfers every run owned by the replaced session. Even a
		// resolved terminal handback may still be present in the daemon spool when
		// the successful HTTP response was lost; it must be able to replay under
		// the replacement session and reach the existing idempotency record.
		_, err = tx.ExecContext(ctx, s.db.Rebind(`UPDATE remote_runs SET session_id = ?, updated_at = ? WHERE session_id = ?`),
			sessionID, db.TimeValue(now), request.ResumeSessionID)
		return err
	})
	if err != nil {
		return Session{}, fmt.Errorf("register worker: %w", err)
	}
	return Session{SessionID: sessionID, WorkerID: request.WorkerID, Version: version, BuildVersion: request.Negotiation.BuildVersion,
		Capabilities: responseCapabilities, State: state, LeaseExpiresAt: db.StoredTime(leaseExpiresAt), LastCommandAck: lastCommandAck}, nil
}

func validRegistrationID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == ':' || r == '.' {
			continue
		}
		return false
	}
	return true
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
		if (state != "active" && state != "draining" && state != "disabled") || !db.TimeFrom(expires).After(now) {
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
		return Command{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
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
				return invalidf("idempotency key reused for a different command")
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
					return invalidf("effect id already belongs to another fenced run")
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
			return invalidf("start command requires a durable run spec")
		}
		return nil
	}
	if err := spec.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	if envelope.Type != executioncontract.CommandStart {
		return invalidf("run spec is only valid for a start command")
	}
	if spec.RunID != envelope.RunID {
		return invalidf("command and run spec identities differ")
	}
	var start executioncontract.StartCommandPayload
	if err := json.Unmarshal(envelope.Payload, &start); err != nil || start.Spec == nil {
		return invalidf("durable start delivery requires an inline run spec")
	}
	provided, _ := json.Marshal(spec)
	embedded, _ := json.Marshal(start.Spec)
	if !bytes.Equal(provided, embedded) {
		return invalidf("command payload and run spec differ")
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
			return invalidf("command acknowledgement exceeds delivered cursor")
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
	terminalRuns := map[string]bool{}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := s.requireSessionTx(ctx, tx, batch.SessionID, true); err != nil {
			return err
		}
		for i := range batch.Events {
			event := batch.Events[i]
			if err := event.Validate(); err != nil {
				return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
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
					return invalidf("replayed event differs from durable event")
				}
				acks[event.RunID] = current
				continue
			}
			if event.Sequence != current+1 {
				return ErrEventGap
			}
			if state == "terminal" {
				return invalidf("event follows terminal event")
			}
			if action, required, err := approvalAuthorization(event, batch.Authorizations); err != nil {
				return err
			} else if required {
				if action.SessionID != batch.SessionID || action.ReplayKey != event.IdempotencyKey {
					return ErrInvalidRequest
				}
				// This event row is the replay record for approval transport. Keep
				// scope validation non-mutating so a crash before SQL commit cannot
				// strand the daemon behind a consumed grant key.
				if err := s.authorizeRunActionTx(ctx, tx, event.RunID, action); err != nil {
					return err
				}
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
					return invalidf("event stream mixes build or protocol identities")
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
				terminalRuns[event.RunID] = true
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
	if err != nil {
		return acks, err
	}
	for runID := range terminalRuns {
		if revokeErr := s.grants.RevokeRun(runID); revokeErr != nil {
			return acks, revokeErr
		}
	}
	return acks, nil
}

func approvalAuthorization(event executioncontract.EventEnvelope, authorizations map[string]RunActionRequest) (RunActionRequest, bool, error) {
	if event.Type != executioncontract.EventProgress {
		return RunActionRequest{}, false, nil
	}
	var payload struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return RunActionRequest{}, false, fmt.Errorf("%w: decode progress event: %w", ErrInvalidRequest, err)
	}
	if payload.Kind != "approval_request" {
		return RunActionRequest{}, false, nil
	}
	action, ok := authorizations[event.IdempotencyKey]
	if !ok {
		return RunActionRequest{}, false, invalidf("approval event requires scoped authorization")
	}
	return action, true, nil
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
			return invalidf("event acknowledgement exceeds durable cursor")
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
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	if upload.Manifest.State != executioncontract.ArtifactsReady {
		return invalidf("only complete artifact manifests may be staged")
	}
	if _, err := executioncontract.ValidateArtifactPackage(upload.Manifest, upload.Content); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := s.requireSessionTx(ctx, tx, upload.SessionID, true); err != nil {
			return err
		}
		var runSession, specJSON string
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT session_id, run_spec_json FROM remote_runs WHERE run_id = ?`), upload.Manifest.RunID).Scan(&runSession, &specJSON); err != nil {
			return err
		}
		if runSession != upload.SessionID {
			return ErrStaleSession
		}
		var spec executioncontract.RunSpec
		if err := json.Unmarshal([]byte(specJSON), &spec); err != nil {
			return err
		}
		if upload.Manifest.Fence != spec.Fence || upload.Manifest.Workspace.RepositoryID != spec.Workspace.RepositoryID ||
			upload.Manifest.Workspace.BaseSHA != spec.Workspace.BaseSHA || upload.Manifest.Workspace.BaseRef != spec.Workspace.BaseRef {
			return invalidf("artifact handback differs from durable run fence or workspace")
		}
		if err := validateRequiredOutputs(spec, upload.Manifest); err != nil {
			return err
		}
		encoded, _ := json.Marshal(upload.Manifest)
		var existingManifest, existingRun string
		var existingContent []byte
		err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT manifest_json, run_id, content FROM worker_artifacts WHERE run_id = ? ORDER BY imported_at DESC LIMIT 1`),
			upload.Manifest.RunID).Scan(&existingManifest, &existingRun, &existingContent)
		if err == nil {
			if existingManifest != string(encoded) || !bytes.Equal(existingContent, upload.Content) {
				return invalidf("run already has a different artifact handback")
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		err = tx.QueryRowContext(ctx, s.db.Rebind(`SELECT manifest_json, run_id, content FROM worker_artifacts WHERE idempotency_key = ?`),
			upload.Manifest.IdempotencyKey).Scan(&existingManifest, &existingRun, &existingContent)
		if err == nil {
			if existingManifest != string(encoded) || existingRun != upload.Manifest.RunID || !bytes.Equal(existingContent, upload.Content) {
				return invalidf("artifact idempotency key reused for different content")
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
		// "staged" is deliberately distinct from the daemon's "ready" claim.
		// Workflow authority may advance only after the leader importer validates
		// current generations/base ancestry and applies the package.
		_, err = tx.ExecContext(ctx, s.db.Rebind(`UPDATE remote_runs SET artifact_state = 'staged', updated_at = ? WHERE run_id = ?`),
			db.TimeValue(s.now().UTC()), upload.Manifest.RunID)
		return err
	})
	if err != nil {
		return err
	}
	if s.importArtifact != nil {
		// A daemon may retry after the leader imported the handback but its HTTP
		// response was lost. The durable resolution is authoritative: replaying an
		// identical upload must not invoke the importer a second time.
		var artifactState string
		if err := s.db.QueryRowContext(ctx, s.db.Rebind(`SELECT artifact_state FROM remote_runs WHERE run_id = ?`), upload.Manifest.RunID).Scan(&artifactState); err != nil {
			return err
		}
		if artifactState != "staged" && artifactState != "importing" {
			return nil
		}
		if err := s.importArtifact(ctx, upload.Manifest.RunID); err != nil {
			return fmt.Errorf("worker control: import staged artifact: %w", err)
		}
	}
	return nil
}

func validateRequiredOutputs(spec executioncontract.RunSpec, manifest executioncontract.ArtifactManifest) error {
	byName := make(map[string]executioncontract.ArtifactEntry, len(manifest.Artifacts))
	declared := make(map[string]executioncontract.ExpectedOutput, len(spec.ExpectedOutputs))
	for _, expected := range spec.ExpectedOutputs {
		declared[expected.Name] = expected
	}
	for i := range manifest.Artifacts {
		entry := &manifest.Artifacts[i]
		byName[entry.Name] = *entry
		if _, ok := declared[entry.Name]; ok {
			continue
		}
		builtinGit := (entry.Name == "git-bundle" && entry.Kind == "git_bundle" && entry.Root == executioncontract.RootArtifact && entry.Path == "git/commits.bundle") ||
			(entry.Name == "git-staged-patch" && entry.Kind == "git_staged_patch" && entry.Root == executioncontract.RootArtifact && entry.Path == "git/staged.patch") ||
			(entry.Name == "git-unstaged-patch" && entry.Kind == "git_unstaged_patch" && entry.Root == executioncontract.RootArtifact && entry.Path == "git/unstaged.patch") ||
			(strings.HasPrefix(entry.Name, "untracked:") && entry.Kind == "git_untracked" && entry.Root == executioncontract.RootWorktree && entry.Name == "untracked:"+entry.Path)
		if !builtinGit {
			return invalidf("artifact %q was not declared by the run", entry.Name)
		}
	}
	for _, expected := range spec.ExpectedOutputs {
		entry, present := byName[expected.Name]
		if !present {
			if expected.Required {
				return invalidf("required artifact %q is missing", expected.Name)
			}
			continue
		}
		if entry.Kind != expected.Kind || entry.Root != expected.Root || entry.Path != expected.Path || entry.Sensitivity != expected.Sensitivity {
			return invalidf("artifact %q differs from its declared output", expected.Name)
		}
		limit := expected.MaxBytes
		if limit == 0 {
			limit = executioncontract.MaxArtifactEntrySize
		}
		if entry.SizeBytes > limit || (len(expected.MediaTypes) > 0 && !slices.Contains(expected.MediaTypes, entry.MediaType)) {
			return invalidf("artifact %q exceeds its declared size or media-type policy", expected.Name)
		}
	}
	return nil
}

// LoadStagedArtifact returns a fully byte-validated package for the
// leader-owned generation/base importer. Loading does not authorize workflow
// advancement or mutate canonical state.
func (s *Service) LoadStagedArtifact(ctx context.Context, runID string) (ArtifactHandback, error) {
	var specJSON, manifestJSON, state string
	var content []byte
	err := s.db.QueryRowContext(ctx, s.db.Rebind(`SELECT r.run_spec_json, r.artifact_state, a.manifest_json, a.content
		FROM remote_runs r JOIN worker_artifacts a ON a.run_id = r.run_id
		WHERE r.run_id = ? ORDER BY a.imported_at DESC LIMIT 1`), runID).
		Scan(&specJSON, &state, &manifestJSON, &content)
	if err != nil {
		return ArtifactHandback{}, err
	}
	if state != "staged" && state != "importing" {
		return ArtifactHandback{}, invalidf("artifact handback is not importable")
	}
	var out ArtifactHandback
	if err := json.Unmarshal([]byte(specJSON), &out.Spec); err != nil {
		return ArtifactHandback{}, err
	}
	if err := json.Unmarshal([]byte(manifestJSON), &out.Manifest); err != nil {
		return ArtifactHandback{}, err
	}
	out.Package, err = executioncontract.ValidateArtifactPackage(out.Manifest, content)
	if err != nil {
		return ArtifactHandback{}, err
	}
	if err := validateRequiredOutputs(out.Spec, out.Manifest); err != nil {
		return ArtifactHandback{}, err
	}
	out.Content = append([]byte(nil), content...)
	out.State = state
	return out, nil
}

// BeginArtifactImport durably journals publication before canonical state is
// touched. Repeating it after a leader crash is harmless and lets the importer
// reconcile an operation that may have stopped at any publication boundary.
func (s *Service) BeginArtifactImport(ctx context.Context, runID, manifestID string) error {
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		var current, storedManifest string
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT r.artifact_state, a.manifest_id FROM remote_runs r JOIN worker_artifacts a ON a.run_id = r.run_id WHERE r.run_id = ? ORDER BY a.imported_at DESC LIMIT 1`), runID).Scan(&current, &storedManifest); err != nil {
			return err
		}
		if storedManifest != manifestID {
			return invalidf("artifact manifest differs from staged handback")
		}
		if current == "importing" {
			return nil
		}
		if current != "staged" {
			return invalidf("artifact handback is not staged")
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE remote_runs SET artifact_state = 'importing', updated_at = ? WHERE run_id = ? AND artifact_state = 'staged'`), db.TimeValue(s.now().UTC()), runID)
		return err
	})
}

// ResolveArtifact records the leader importer's terminal decision. A staged
// package remains diagnostic/private until retention pruning; it is never
// treated as imported merely because the daemon called it ready.
func (s *Service) ResolveArtifact(ctx context.Context, runID, manifestID, state string) error {
	if state != "imported" && state != "rejected" {
		return invalidf("artifact resolution must be imported or rejected")
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		var current, storedManifest string
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT r.artifact_state, a.manifest_id FROM remote_runs r JOIN worker_artifacts a ON a.run_id = r.run_id WHERE r.run_id = ? ORDER BY a.imported_at DESC LIMIT 1`), runID).Scan(&current, &storedManifest); err != nil {
			return err
		}
		if storedManifest != manifestID {
			return invalidf("artifact manifest differs from staged handback")
		}
		if current == state {
			return nil
		}
		if current != "staged" && current != "importing" {
			return invalidf("artifact handback is already resolved")
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE remote_runs SET artifact_state = ?, updated_at = ? WHERE run_id = ? AND artifact_state IN ('staged', 'importing')`), state, db.TimeValue(s.now().UTC()), runID)
		return err
	})
}

func (s *Service) PruneResolvedArtifacts(ctx context.Context, before time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, s.db.Rebind(`DELETE FROM worker_artifacts WHERE imported_at < ? AND run_id IN
		(SELECT run_id FROM remote_runs WHERE artifact_state IN ('imported', 'rejected'))`), db.TimeValue(before.UTC()))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
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

// SetWorkerDisabled is the durable operator kill switch for new placement.
// Accepted work may still report events and hand back artifacts; disabled
// workers are excluded from scheduling until explicitly enabled.
func (s *Service) SetWorkerDisabled(ctx context.Context, workerID string, disabled bool) error {
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		return invalidf("worker is required")
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error { return s.setWorkerDisabledTx(ctx, tx, workerID, disabled) })
}

// SetSessionDisabled is the worker-facing form: a session may only disable its
// own stable worker identity, never another fleet member.
func (s *Service) SetSessionDisabled(ctx context.Context, sessionID string, disabled bool) error {
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := s.requireSessionTx(ctx, tx, sessionID, true); err != nil {
			return err
		}
		var workerID string
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT worker_id FROM worker_sessions WHERE session_id = ?`), sessionID).Scan(&workerID); err != nil {
			return err
		}
		return s.setWorkerDisabledTx(ctx, tx, workerID, disabled)
	})
}

func (s *Service) setWorkerDisabledTx(ctx context.Context, tx *sql.Tx, workerID string, disabled bool) error {
	if disabled {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO worker_disabled (worker_id, disabled_at) VALUES (?, ?) ON CONFLICT(worker_id) DO UPDATE SET disabled_at = excluded.disabled_at`), workerID, db.TimeValue(s.now().UTC())); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE worker_sessions SET state = 'disabled' WHERE worker_id = ? AND state IN ('active', 'draining')`), workerID)
		return err
	}
	if _, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM worker_disabled WHERE worker_id = ?`), workerID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE worker_sessions SET state = 'active' WHERE session_id = (SELECT session_id FROM worker_sessions WHERE worker_id = ? AND state = 'disabled' ORDER BY created_at DESC LIMIT 1)`), workerID)
	return err
}

func (s *Service) Diagnostics(ctx context.Context) ([]Diagnostics, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.worker_id, s.session_id, s.state, s.build_version, s.protocol_major,
		s.protocol_minor, s.lease_expires_at, s.last_command_ack, s.capabilities_json,
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
		var capabilitiesJSON string
		if err := rows.Scan(&item.WorkerID, &item.SessionID, &item.State, &item.BuildVersion, &major, &minor, &lease,
			&item.LastCommandAck, &capabilitiesJSON, &item.ActiveRuns, &item.PendingEvents); err != nil {
			return nil, err
		}
		item.Protocol = fmt.Sprintf("%d.%d", major, minor)
		item.LeaseExpiresAt = db.TimeFrom(lease)
		var capabilities []string
		if err := json.Unmarshal([]byte(capabilitiesJSON), &capabilities); err != nil {
			return nil, fmt.Errorf("decode capabilities for worker %q session %q: %w", item.WorkerID, item.SessionID, err)
		}
		parsed := parseCapabilities(capabilities)
		item.Capacity = parsed.capacity
		item.SpoolBytes, _ = strconv.ParseInt(parsed.one("spool_bytes"), 10, 64)
		item.SpoolMaxBytes, _ = strconv.ParseInt(parsed.one("spool_max_bytes"), 10, 64)
		item.AvailableCapacity = max(item.Capacity-item.ActiveRuns, 0)
		// max-max/5 is ceil(80% of max) without overflowing large counters.
		if item.SpoolMaxBytes > 0 && item.SpoolBytes >= item.SpoolMaxBytes-item.SpoolMaxBytes/5 {
			item.Alerts = append(item.Alerts, "spool_pressure")
		}
		if item.PendingEvents > 0 {
			item.Alerts = append(item.Alerts, "unacknowledged_events")
		}
		if item.Capacity > 0 && item.AvailableCapacity == 0 {
			item.Alerts = append(item.Alerts, "capacity_saturated")
		}
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
	if result.State != "active" && result.State != "draining" && result.State != "disabled" {
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
	if state != "active" && (!allowDraining || (state != "draining" && state != "disabled")) {
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

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidRequest, fmt.Sprintf(format, args...))
}
