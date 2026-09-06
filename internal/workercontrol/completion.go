package workercontrol

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/executioncontract"
)

var ErrCompletionUnproven = errors.New("remote completion has no matching durable receipt")

// TerminalReceipt identifies the exact immutable event consumed by the leader.
// It contains no provider text. An observer timeout or malformed terminal event
// cannot mint a receipt. The receipt becomes evidence only when the leader
// stores it atomically with the canonical run result and cost.
func TerminalReceipt(event executioncontract.EventEnvelope) string {
	if event.Type != executioncontract.EventTerminal || event.Validate() != nil {
		return ""
	}
	var terminal struct {
		State             executioncontract.TerminalState `json:"state"`
		ArtifactState     executioncontract.ArtifactState `json:"artifactState"`
		Error             string                          `json:"error"`
		ArtifactError     string                          `json:"artifactError,omitempty"`
		Permanent         bool                            `json:"permanent,omitempty"`
		AdmissionDeferred bool                            `json:"admissionDeferred,omitempty"`
	}
	if json.Unmarshal(event.Payload, &terminal) != nil {
		return ""
	}
	switch terminal.State {
	case executioncontract.TerminalSucceeded, executioncontract.TerminalFailed, executioncontract.TerminalCanceled:
	default:
		return ""
	}
	switch terminal.ArtifactState {
	case executioncontract.ArtifactsReady, executioncontract.ArtifactsFailed, executioncontract.ArtifactsPending:
	default:
		return ""
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("v1:%x", sha256.Sum256(encoded))
}

// PendingResult contains only the identity and disposition needed by the
// canonical leader. This projection contains no provider output.
type PendingResult struct {
	RunID, TaskID, Receipt, ArtifactState string
	TerminalArtifactState                 executioncontract.ArtifactState
	Through                               uint64
	AcknowledgedThrough                   uint64
	PendingEvents                         int
}

func (r PendingResult) HoldReason(receipt string) string {
	if r.Receipt == "" {
		return "invalid_terminal"
	}
	if receipt == "" {
		return "missing_completion_receipt"
	}
	if receipt != r.Receipt {
		return "completion_receipt_mismatch"
	}
	if r.TerminalArtifactState == executioncontract.ArtifactsReady &&
		r.ArtifactState != "imported" && r.ArtifactState != "rejected" {
		return "artifact_unresolved"
	}
	if r.TerminalArtifactState == executioncontract.ArtifactsPending {
		return "artifact_unresolved"
	}
	return ""
}

const pendingResultColumns = `r.run_id, r.task_id, r.artifact_state, r.last_event_sequence, r.last_event_ack,
	e.envelope_json, (SELECT COUNT(*) FROM worker_events p WHERE p.run_id = r.run_id AND p.acknowledged_at IS NULL)`

const pendingResultJoin = ` FROM remote_runs r JOIN worker_events e
	ON e.run_id = r.run_id AND e.sequence = r.last_event_sequence `

type resultScanner interface{ Scan(...any) error }

func scanPendingResult(row resultScanner) (PendingResult, error) {
	var result PendingResult
	var encoded string
	if err := row.Scan(&result.RunID, &result.TaskID, &result.ArtifactState, &result.Through, &result.AcknowledgedThrough, &encoded, &result.PendingEvents); err != nil {
		return result, err
	}
	var event executioncontract.EventEnvelope
	if json.Unmarshal([]byte(encoded), &event) == nil && event.RunID == result.RunID && event.Sequence == result.Through {
		result.Receipt = TerminalReceipt(event)
		var terminal struct {
			ArtifactState executioncontract.ArtifactState `json:"artifactState"`
		}
		if json.Unmarshal(event.Payload, &terminal) == nil {
			result.TerminalArtifactState = terminal.ArtifactState
		}
	}
	return result, nil
}

// PendingResults pages by immutable run identity, not mutable session ownership.
// It never acknowledges, imports artifacts, or changes a task.
func (s *Service) PendingResults(ctx context.Context, after string, limit int) ([]PendingResult, error) {
	if limit < 1 || limit > 100 {
		return nil, invalidf("result recovery limit must be between 1 and 100")
	}
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(`SELECT `+pendingResultColumns+pendingResultJoin+
		`WHERE r.state = 'terminal' AND r.last_event_ack < r.last_event_sequence AND r.run_id > ? ORDER BY r.run_id LIMIT ?`), after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	results := []PendingResult{}
	for rows.Next() {
		result, err := scanPendingResult(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, rows.Err()
}

func (s *Service) ResultForRun(ctx context.Context, runID string) (PendingResult, error) {
	return scanPendingResult(s.db.QueryRowContext(ctx, s.db.Rebind(`SELECT `+pendingResultColumns+pendingResultJoin+
		`WHERE r.state = 'terminal' AND r.run_id = ?`), runID))
}

// AcknowledgeCompletedResult is leader-only: receipt must come from the
// canonical run store, never from a worker request. No HTTP endpoint exposes
// this method. Unlike transport ACKs, it does not require a live worker lease:
// a completed result belongs to the leader even after that worker is replaced.
func (s *Service) AcknowledgeCompletedResult(ctx context.Context, runID, receipt string) (bool, error) {
	changed := false
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		query := `SELECT ` + pendingResultColumns + pendingResultJoin + `WHERE r.state = 'terminal' AND r.run_id = ?`
		if s.db.Dialect() == db.Postgres {
			query += ` FOR UPDATE OF r`
		}
		result, err := scanPendingResult(tx.QueryRowContext(ctx, s.db.Rebind(query), runID))
		if err != nil {
			return err
		}
		if result.HoldReason(receipt) != "" {
			return ErrCompletionUnproven
		}
		// PostgreSQL can evaluate the correlated event count from the statement
		// snapshot before waiting for this row lock. Only the locked row's ACK
		// cursor is authoritative for concurrent-consumer idempotence.
		if result.AcknowledgedThrough >= result.Through {
			return nil
		}
		now := db.TimeValue(s.now().UTC())
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE worker_events SET acknowledged_at = COALESCE(acknowledged_at, ?) WHERE run_id = ? AND sequence <= ?`), now, runID, result.Through); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, s.db.Rebind(`UPDATE remote_runs SET last_event_ack = ? WHERE run_id = ?`), result.Through, runID)
		changed = err == nil
		return err
	})
	return changed && err == nil, err
}
