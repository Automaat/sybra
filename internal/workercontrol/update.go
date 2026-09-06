package workercontrol

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/version"
)

var (
	ErrUpdateHeld        = errors.New("worker control: update hold condition not satisfied")
	ErrUpdateUnavailable = errors.New("worker control: no approved worker release")
)

func (s *Service) requireNoUpdateHoldTx(ctx context.Context, tx *sql.Tx, sessionID string) error {
	held, err := s.updateHeldTx(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	if held {
		return ErrUpdateHeld
	}
	return nil
}

type WorkerRelease struct {
	Revision string                    `json:"revision"`
	Protocol executioncontract.Version `json:"protocol"`
}

// Called after placement locks its sessions, just like the reservation count.
// A fresh statement observes a hold committed while waiting for those locks.
func (s *Service) updateHeldWorkersTx(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT worker_id FROM worker_update_holds`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	held := make(map[string]bool)
	for rows.Next() {
		var workerID string
		if err := rows.Scan(&workerID); err != nil {
			return nil, err
		}
		held[workerID] = true
	}
	return held, rows.Err()
}

// UpdateHold is separate from worker_disabled: finishing an update must never
// undo an operator's disable/drain decision. Holds have no automatic expiry;
// an interrupted updater resumes its durable journal or rolls back first.
type UpdateHold struct {
	WorkerID         string    `json:"workerId"`
	ID               string    `json:"id"`
	Revision         string    `json:"revision"`
	PreviousRevision string    `json:"previousRevision"`
	StartedAt        time.Time `json:"startedAt"`
}

type UpdateRequest struct {
	WorkerID  string `json:"workerId,omitempty"`
	SessionID string `json:"sessionId"`
	ID        string `json:"id"`
	Revision  string `json:"revision"`
}

// CheckUpdateOwnership permits recovery when the replacement cannot register.
// It proves the exact stable-worker hold still exists and there is no accepted
// work, without mistaking a missing live session for authorization to restart.
func (s *Service) CheckUpdateOwnership(ctx context.Context, request UpdateRequest) error {
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		var id, revision string
		query := `SELECT hold_id, target_revision FROM worker_update_holds WHERE worker_id = ?`
		if s.db.Dialect() == db.Postgres {
			query += ` FOR UPDATE`
		}
		err := tx.QueryRowContext(ctx, s.db.Rebind(query), request.WorkerID).Scan(&id, &revision)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUpdateHeld
		}
		if err != nil {
			return err
		}
		if id != request.ID || revision != request.Revision {
			return ErrUpdateHeld
		}
		var active int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM remote_runs WHERE worker_id = ? AND state != 'terminal'`), request.WorkerID).Scan(&active); err != nil {
			return err
		}
		if active != 0 {
			return ErrUpdateHeld
		}
		return nil
	})
}

// SetUpdateRevision is initialization-only, before Handler is served. The
// value comes from the running binary, never from a mutable checkout or ref.
func (s *Service) SetUpdateRevision(revision string) {
	if version.ValidRevision(revision) {
		s.updateRevision = revision
	}
}

func (s *Service) WorkerRelease() (WorkerRelease, error) {
	if s.updateRevision == "" {
		return WorkerRelease{}, ErrUpdateUnavailable
	}
	return WorkerRelease{Revision: s.updateRevision, Protocol: executioncontract.CurrentVersion()}, nil
}

// BeginUpdate serializes with both scheduling paths on the session row. It
// stops new reservations, not already accepted work or its event delivery.
// The caller persists its random ID before asking, making a lost reply safe.
func (s *Service) BeginUpdate(ctx context.Context, request UpdateRequest) (UpdateHold, error) {
	if len(request.ID) != 32 || !version.ValidRevision(request.Revision) {
		return UpdateHold{}, invalidf("invalid update identity")
	}
	if _, err := hex.DecodeString(request.ID); err != nil {
		return UpdateHold{}, invalidf("invalid update identity")
	}
	var hold UpdateHold
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := s.requireSessionTx(ctx, tx, request.SessionID, true); err != nil {
			return err
		}
		var state string
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT worker_id, build_version, state FROM worker_sessions WHERE session_id = ?`), request.SessionID).
			Scan(&hold.WorkerID, &hold.PreviousRevision, &state); err != nil {
			return err
		}
		existing, err := s.updateHoldTx(ctx, tx, hold.WorkerID)
		if err == nil {
			if existing.ID != request.ID || existing.Revision != request.Revision {
				return ErrUpdateHeld
			}
			hold = existing
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if state != "active" || !version.ValidRevision(hold.PreviousRevision) {
			return ErrUpdateUnavailable
		}
		if request.Revision != s.updateRevision || s.updateRevision == "" {
			return ErrUpdateUnavailable
		}
		hold.ID, hold.Revision, hold.StartedAt = request.ID, request.Revision, db.StoredTime(s.now().UTC())
		_, err = tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO worker_update_holds (worker_id, hold_id, target_revision, previous_revision, started_at) VALUES (?, ?, ?, ?, ?)`),
			hold.WorkerID, hold.ID, hold.Revision, hold.PreviousRevision, db.TimeValue(hold.StartedAt))
		return err
	})
	return hold, err
}

// FinishUpdate releases only the caller's hold after a healthy target or
// retained rollback build is registered. Disabled/draining state is untouched.
func (s *Service) FinishUpdate(ctx context.Context, request UpdateRequest) error {
	return s.checkOrFinishUpdate(ctx, request, true)
}

// CheckUpdate proves quiescence across every session of the stable worker,
// including an old reservation not yet transferred to a replacement session.
func (s *Service) CheckUpdate(ctx context.Context, request UpdateRequest) error {
	return s.checkOrFinishUpdate(ctx, request, false)
}

func (s *Service) checkOrFinishUpdate(ctx context.Context, request UpdateRequest, finish bool) error {
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := s.requireSessionTx(ctx, tx, request.SessionID, true); err != nil {
			return err
		}
		var workerID, build, encoded string
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT worker_id, build_version, capabilities_json FROM worker_sessions WHERE session_id = ?`), request.SessionID).
			Scan(&workerID, &build, &encoded); err != nil {
			return err
		}
		hold, err := s.updateHoldTx(ctx, tx, workerID)
		if errors.Is(err, sql.ErrNoRows) {
			if !finish {
				return ErrUpdateHeld
			}
			return nil // lost successful reply; never touches a different hold
		}
		if err != nil {
			return err
		}
		if hold.ID != request.ID || hold.Revision != request.Revision || (build != hold.Revision && build != hold.PreviousRevision) {
			return ErrUpdateHeld
		}
		var capabilities []string
		if err := json.Unmarshal([]byte(encoded), &capabilities); err != nil {
			return err
		}
		parsed := parseCapabilities(capabilities)
		if parsed.one("readiness") != "ready" || parsed.one("buffered_events") != "0" || parsed.one("pending_artifacts") != "0" {
			return ErrUpdateHeld
		}
		var active int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM remote_runs WHERE worker_id = ? AND state != 'terminal'`), workerID).Scan(&active); err != nil {
			return err
		}
		if active != 0 {
			return ErrUpdateHeld
		}
		if !finish {
			return nil
		}
		_, err = tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM worker_update_holds WHERE worker_id = ? AND hold_id = ?`), workerID, hold.ID)
		return err
	})
}

func (s *Service) updateHoldTx(ctx context.Context, tx *sql.Tx, workerID string) (UpdateHold, error) {
	var hold UpdateHold
	var at int64
	err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT worker_id, hold_id, target_revision, previous_revision, started_at FROM worker_update_holds WHERE worker_id = ?`), workerID).
		Scan(&hold.WorkerID, &hold.ID, &hold.Revision, &hold.PreviousRevision, &at)
	hold.StartedAt = db.TimeFrom(at)
	return hold, err
}

func (s *Service) updateHeldTx(ctx context.Context, tx *sql.Tx, sessionID string) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM worker_update_holds h JOIN worker_sessions s ON s.worker_id = h.worker_id WHERE s.session_id = ?`), sessionID).Scan(&count)
	return count != 0, err
}
