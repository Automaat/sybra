// Package toolledger records every tool call an agent makes, independent of
// the permission posture in force.
//
// The raw provider stream already contains this data, but as a pile of
// per-run files in per-provider wire formats with a short retention — usable
// for debugging one run, not for deriving a policy across many. The ledger is
// the distilled form: one normalized record per tool call, retained on its own
// schedule.
//
// Deliberately not gated on anything. A ledger that only fills up under one
// posture produces a policy fitted to that posture, and leaves the case where
// a human is personally adjudicating tool calls — the most informative one —
// undocumented.
package toolledger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Record is one tool call as it was requested.
type Record struct {
	Timestamp time.Time `json:"ts"`
	AgentID   string    `json:"agentId"`
	TaskID    string    `json:"taskId,omitempty"`
	Role      string    `json:"role,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	Tool      string    `json:"tool"`
	// ToolUseID joins the two records one call can produce: an observation
	// from the provider stream, and a decision from the PreToolUse hook.
	// Without it, mining an approval-posture window double-counts every
	// allowed call.
	ToolUseID string         `json:"toolUseId,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	// Decision is empty for an observation, or the verdict a permission layer
	// reached ("allow"/"deny"). Approvals matter as much as refusals: a corpus
	// of only refusals describes what a human rejected and nothing about the
	// far larger set they waved through.
	Decision string `json:"decision,omitempty"`
	// DecidedBy names what produced Decision (human, safe-tool fast path,
	// policy), so a later policy can tell a considered approval from an
	// automatic one.
	DecidedBy string `json:"decidedBy,omitempty"`
}

// Logger appends records to date-named NDJSON files under dir.
type Logger struct {
	dir string

	mu      sync.Mutex
	current *os.File
	today   string
}

// New returns a Logger writing under dir, creating it if absent.
func New(dir string) (*Logger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Logger{dir: dir}, nil
}

// Log appends one record. A nil Logger is a no-op, so callers on the hot
// stream path need no guard.
//
// The UTC conversion is load-bearing rather than cosmetic: the file name is
// derived from this timestamp's date, so a record stamped in a local zone
// would be filed under a date a UTC-based reader never looks for (the same
// trap internal/audit carried).
func (l *Logger) Log(r Record) error {
	if l == nil {
		return nil
	}
	if r.Timestamp.IsZero() {
		r.Timestamp = time.Now()
	}
	r.Timestamp = r.Timestamp.UTC()

	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := l.file(r.Timestamp)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return err
}

func (l *Logger) file(ts time.Time) (*os.File, error) {
	day := ts.Format(time.DateOnly)
	if l.current != nil && l.today == day {
		return l.current, nil
	}
	if l.current != nil {
		_ = l.current.Close()
	}
	f, err := os.OpenFile(filepath.Join(l.dir, day+".ndjson"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("toolledger: open %s: %w", day, err)
	}
	l.current, l.today = f, day
	return f, nil
}

// Close releases the open file. Safe on a nil Logger.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.current == nil {
		return nil
	}
	err := l.current.Close()
	l.current, l.today = nil, ""
	return err
}
