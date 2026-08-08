package loopagent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/providerid"
)

// ErrNotFound reports a loop agent id with no record behind it. It wraps
// sql.ErrNoRows so callers can use either.
var ErrNotFound = fmt.Errorf("loop agent not found: %w", sql.ErrNoRows)

// SQLStore persists loop agents in the configured database backend.
//
// It needs no file locks: a read-modify-write runs inside one transaction, so
// a concurrent writer in another process is serialized by the engine rather
// than by an advisory lock that dies with the process holding it.
type SQLStore struct {
	db *db.DB
}

// NewSQLStore returns a database-backed loop agent repository.
func NewSQLStore(database *db.DB) (*SQLStore, error) {
	if database == nil {
		return nil, errors.New("loop agent sql store needs an open database")
	}
	return &SQLStore{db: database}, nil
}

const loopAgentColumns = `id, name, prompt, interval_sec, allowed_tools, provider, model, enabled,
	last_run_at, last_run_id, last_run_cost, created_at, updated_at`

const selectLoopAgents = `SELECT ` + loopAgentColumns + ` FROM loop_agents ORDER BY name, id`

const selectLoopAgent = `SELECT ` + loopAgentColumns + ` FROM loop_agents WHERE id = ?`

const insertLoopAgent = `INSERT INTO loop_agents (` + loopAgentColumns + `)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const updateLoopAgent = `UPDATE loop_agents SET name = ?, prompt = ?, interval_sec = ?, allowed_tools = ?,
	provider = ?, model = ?, enabled = ?, last_run_at = ?, last_run_id = ?, last_run_cost = ?, updated_at = ?
	WHERE id = ?`

// List returns all loop agents sorted by Name for stable UI ordering.
func (s *SQLStore) List() ([]LoopAgent, error) {
	ctx := context.Background()
	rows, err := s.db.QueryContext(ctx, selectLoopAgents)
	if err != nil {
		return nil, fmt.Errorf("list loop agents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]LoopAgent, 0, 8)
	for rows.Next() {
		la, err := scanLoopAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, la)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate loop agents: %w", err)
	}
	return out, nil
}

// Get returns the loop agent with the given ID.
func (s *SQLStore) Get(id string) (LoopAgent, error) {
	return s.get(context.Background(), s.db, id)
}

// FindByName returns the first loop agent whose Name matches. Used by the first-boot seed to stay idempotent.
func (s *SQLStore) FindByName(name string) (LoopAgent, bool) {
	all, err := s.List()
	if err != nil {
		return LoopAgent{}, false
	}
	for i := range all {
		if all[i].Name == name {
			return all[i], true
		}
	}
	return LoopAgent{}, false
}

// Create assigns an ID and timestamps, validates, and inserts the record.
func (s *SQLStore) Create(la LoopAgent) (LoopAgent, error) {
	if la.Provider == "" {
		la.Provider = providerid.Claude
	}
	if err := la.Validate(); err != nil {
		return LoopAgent{}, err
	}
	now := time.Now().UTC()
	la.ID = newID()
	la.CreatedAt = now
	la.UpdatedAt = now

	tools, err := marshalTools(la.AllowedTools)
	if err != nil {
		return LoopAgent{}, err
	}
	_, err = s.db.ExecContext(context.Background(), insertLoopAgent,
		la.ID, la.Name, la.Prompt, la.IntervalSec, tools, la.Provider, la.Model, db.BoolValue(la.Enabled),
		db.TimeValue(la.LastRunAt), la.LastRunID, la.LastRunCost, db.TimeValue(la.CreatedAt), db.TimeValue(la.UpdatedAt))
	if err != nil {
		return LoopAgent{}, fmt.Errorf("create loop agent: %w", err)
	}
	return la, nil
}

// Update overwrites mutable fields on an existing record. ID and CreatedAt are preserved from the stored version regardless of caller input.
func (s *SQLStore) Update(la LoopAgent) (LoopAgent, error) {
	return s.mutate(la.ID, func(existing *LoopAgent) error {
		created := existing.CreatedAt
		next := la
		next.CreatedAt = created
		next.UpdatedAt = time.Now().UTC()
		if next.Provider == "" {
			next.Provider = existing.Provider
		}
		if err := next.Validate(); err != nil {
			return err
		}
		*existing = next
		return nil
	})
}

// UpdateRunMetadata applies mutate to the stored record without bumping UpdatedAt — that field tracks user config changes only, and bumping it here would trip Sync's change detection and restart the fetcher on every fire.
func (s *SQLStore) UpdateRunMetadata(id string, mutate func(*LoopAgent)) (LoopAgent, error) {
	return s.mutate(id, func(existing *LoopAgent) error {
		mutate(existing)
		return nil
	})
}

// Delete removes the record. A missing record is not an error.
func (s *SQLStore) Delete(id string) error {
	if _, err := s.db.ExecContext(context.Background(), `DELETE FROM loop_agents WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete loop agent: %w", err)
	}
	return nil
}

// mutate runs a read-modify-write inside one transaction so two concurrent updates cannot lose one of them.
func (s *SQLStore) mutate(id string, apply func(*LoopAgent) error) (LoopAgent, error) {
	ctx := context.Background()
	var out LoopAgent
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		existing, err := s.get(ctx, txQuerier{tx: tx, rebind: s.db.Rebind}, id)
		if err != nil {
			return err
		}
		if err := apply(&existing); err != nil {
			return err
		}
		tools, err := marshalTools(existing.AllowedTools)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, s.db.Rebind(updateLoopAgent),
			existing.Name, existing.Prompt, existing.IntervalSec, tools, existing.Provider, existing.Model,
			db.BoolValue(existing.Enabled), db.TimeValue(existing.LastRunAt), existing.LastRunID,
			existing.LastRunCost, db.TimeValue(existing.UpdatedAt), existing.ID)
		if err != nil {
			return fmt.Errorf("update loop agent: %w", err)
		}
		out = existing
		return nil
	})
	if err != nil {
		return LoopAgent{}, err
	}
	return out, nil
}

// rowQuerier is the read surface shared by the pool and an open transaction.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type txQuerier struct {
	tx     *sql.Tx
	rebind func(string) string
}

func (t txQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, t.rebind(query), args...)
}

func (s *SQLStore) get(ctx context.Context, q rowQuerier, id string) (LoopAgent, error) {
	la, err := scanLoopAgent(q.QueryRowContext(ctx, selectLoopAgent, id))
	if errors.Is(err, sql.ErrNoRows) {
		return LoopAgent{}, fmt.Errorf("%w (id %s)", ErrNotFound, id)
	}
	return la, err
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanLoopAgent(sc scanner) (LoopAgent, error) {
	var (
		la        LoopAgent
		tools     string
		enabled   int64
		lastRunAt int64
		createdAt int64
		updatedAt int64
	)
	err := sc.Scan(&la.ID, &la.Name, &la.Prompt, &la.IntervalSec, &tools, &la.Provider, &la.Model,
		&enabled, &lastRunAt, &la.LastRunID, &la.LastRunCost, &createdAt, &updatedAt)
	if err != nil {
		return LoopAgent{}, err
	}
	if err := json.Unmarshal([]byte(tools), &la.AllowedTools); err != nil {
		return LoopAgent{}, fmt.Errorf("parse allowed_tools for loop agent %s: %w", la.ID, err)
	}
	la.Enabled = db.BoolFrom(enabled)
	la.LastRunAt = db.TimeFrom(lastRunAt)
	la.CreatedAt = db.TimeFrom(createdAt)
	la.UpdatedAt = db.TimeFrom(updatedAt)
	if la.Provider == "" {
		la.Provider = providerid.Claude
	}
	return la, nil
}

// marshalTools keeps a nil AllowedTools distinct from an empty one, so a record round-trips through the database exactly as it round-tripped through YAML.
func marshalTools(tools []string) (string, error) {
	data, err := json.Marshal(tools)
	if err != nil {
		return "", fmt.Errorf("encode allowed_tools: %w", err)
	}
	return string(data), nil
}
