package selfmonitor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Ledger verdict constants. Kept as plain strings rather than a named type so
// downstream JSON consumers (CLI, GUI) can round-trip them without enum
// marshaling ceremony.
const (
	VerdictConfirmed     = "confirmed"
	VerdictFalsePositive = "false_positive"
	VerdictNeedsHuman    = "needs_human"
	VerdictSuppressed    = "suppressed"
	VerdictPending       = "pending"
)

// LedgerEntry is one appended row in the selfmonitor ledger. The ledger is
// the persisted, cross-restart memory of the selfmonitor loop: what
// fingerprints it has seen, what verdicts the judge handed down, what
// autonomous actions it took, and whether filed issues have been resolved.
//
// Schema is append-only; add new fields with omitempty so existing rows keep
// round-tripping. Never rename or remove fields.
type LedgerEntry struct {
	Fingerprint string    `json:"fingerprint"`
	Category    string    `json:"category,omitempty"`
	TaskID      string    `json:"taskId,omitempty"`
	Verdict     string    `json:"verdict"`
	IssueNumber int       `json:"issueNumber,omitempty"`
	IssueState  string    `json:"issueState,omitempty"`
	Action      string    `json:"action,omitempty"`
	ActionRef   string    `json:"actionRef,omitempty"`
	DryRun      bool      `json:"dryRun,omitempty"`
	Confidence  float64   `json:"confidence,omitempty"`
	Summary     string    `json:"summary,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	ResolvedAt  time.Time `json:"resolvedAt,omitzero"`
}

// Ledger is a thread-safe, append-only JSONL store indexed in-memory. It is
// cheap to Open (replays the file once on startup) and O(1) for Append and
// Latest lookups.
type Ledger struct {
	path string

	mu      sync.RWMutex
	entries []LedgerEntry
	// byFingerprint points to the most recent entry index for each
	// fingerprint. Multiple entries may exist in the log; this field always
	// resolves to the latest observation.
	byFingerprint map[string]int
}

// Open reads the ledger at path (creating the parent directory if missing)
// and returns a ready-to-use Ledger. Malformed rows are silently skipped so
// a partially-flushed append cannot break future startups.
func Open(path string) (*Ledger, error) {
	if path == "" {
		return nil, errors.New("ledger path is empty")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create ledger dir: %w", err)
	}
	l := &Ledger{
		path:          path,
		byFingerprint: map[string]int{},
	}
	if err := l.replay(); err != nil {
		return nil, err
	}
	return l, nil
}

// Path returns the ledger file path — useful for logs and tests.
func (l *Ledger) Path() string { return l.path }

// Len returns the total number of entries currently indexed. Mostly used by
// tests and reporting.
func (l *Ledger) Len() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.entries)
}

// Append writes an entry to disk and updates the in-memory index. CreatedAt
// is stamped to now (UTC) if the caller left it zero.
func (l *Ledger) Append(e LedgerEntry) error {
	if e.Fingerprint == "" {
		return errors.New("ledger entry missing fingerprint")
	}
	if e.Verdict == "" {
		e.Verdict = VerdictPending
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}

	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal entry: %w", err)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open ledger: %w", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write ledger: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync ledger: %w", err)
	}

	l.entries = append(l.entries, e)
	l.byFingerprint[e.Fingerprint] = len(l.entries) - 1
	return nil
}

// Latest returns the most recent entry recorded for a fingerprint, or
// (zero-value, false) if none exists.
func (l *Ledger) Latest(fp string) (LedgerEntry, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	idx, ok := l.byFingerprint[fp]
	if !ok {
		return LedgerEntry{}, false
	}
	return l.entries[idx], true
}

// History returns all entries for a fingerprint whose CreatedAt falls within
// [now-window, now]. A zero window returns every matching entry.
func (l *Ledger) History(fp string, window time.Duration) []LedgerEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []LedgerEntry
	cutoff := ledgerCutoff(window)
	for i := range l.entries {
		e := &l.entries[i]
		if e.Fingerprint != fp {
			continue
		}
		if !cutoff.IsZero() && e.CreatedAt.Before(cutoff) {
			continue
		}
		out = append(out, *e)
	}
	return out
}

// Entries returns all entries whose CreatedAt falls within [now-window, now].
// A zero window returns every entry.
func (l *Ledger) Entries(window time.Duration) []LedgerEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []LedgerEntry
	cutoff := ledgerCutoff(window)
	for i := range l.entries {
		e := &l.entries[i]
		if !cutoff.IsZero() && e.CreatedAt.Before(cutoff) {
			continue
		}
		out = append(out, *e)
	}
	return out
}

// ShouldAutoSuppress reports whether a fingerprint has been classified as
// false_positive at least `threshold` times within `window`. Used by the
// triage step to short-circuit chronic false positives before they reach the
// LLM judge.
func (l *Ledger) ShouldAutoSuppress(fp string, window time.Duration, threshold int) bool {
	if threshold <= 0 {
		return false
	}
	hist := l.History(fp, window)
	count := 0
	for i := range hist {
		if hist[i].Verdict == VerdictFalsePositive || hist[i].Verdict == VerdictSuppressed {
			count++
		}
	}
	return count >= threshold
}

// OpenIssues returns the latest entry per fingerprint whose IssueNumber > 0
// and IssueState != "closed". The closed-loop verifier iterates this slice
// each tick to re-check GitHub state.
func (l *Ledger) OpenIssues() []LedgerEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []LedgerEntry
	for fp, idx := range l.byFingerprint {
		e := &l.entries[idx]
		if e.IssueNumber <= 0 {
			continue
		}
		if e.IssueState == "closed" {
			continue
		}
		cp := *e
		cp.Fingerprint = fp
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// ActionsInWindow counts the number of appended action records (rows with a
// non-empty Action field) whose CreatedAt falls within the given window. The
// actor uses this for the MaxAutoActionsPerDay guard.
func (l *Ledger) ActionsInWindow(window time.Duration) int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	cutoff := ledgerCutoff(window)
	count := 0
	for i := range l.entries {
		e := &l.entries[i]
		if e.Action == "" {
			continue
		}
		if !cutoff.IsZero() && e.CreatedAt.Before(cutoff) {
			continue
		}
		count++
	}
	return count
}

// replay reads the backing file and rebuilds the in-memory index. Called
// once on Open. Tolerant of malformed lines so a crash mid-write cannot
// brick the ledger.
func (l *Ledger) replay() error {
	f, err := os.Open(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open ledger: %w", err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e LedgerEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		if e.Fingerprint == "" {
			continue
		}
		l.entries = append(l.entries, e)
		l.byFingerprint[e.Fingerprint] = len(l.entries) - 1
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("scan ledger: %w", err)
	}
	return nil
}

func ledgerCutoff(window time.Duration) time.Time {
	if window <= 0 {
		return time.Time{}
	}
	return time.Now().UTC().Add(-window)
}
