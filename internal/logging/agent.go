package logging

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func NewAgentOutputFile(logDir, agentID string) (*os.File, error) {
	dir := filepath.Join(logDir, "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	ts := time.Now().UTC().Format("2006-01-02T15-04-05")
	name := agentID + "-" + ts + ".ndjson"
	return os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// AgentLogPruneReport summarizes one PruneAgentLogs run for callers that
// want to surface metrics. Kept intentionally small; totals are enough for
// the slog line and tests, and the struct is exposed so tests can assert
// the classification without parsing log output.
type AgentLogPruneReport struct {
	Scanned      int
	DeletedOld   int
	DeletedEmpty int
	Kept         int
	Errors       []error
}

// PruneAgentLogs removes per-agent NDJSON files under <logDir>/agents/
// older than maxAge, plus all 0-byte files regardless of age. The 0-byte
// bucket sweeps up runs that failed before the headless streamer ever
// got to write a line (on the synapse server, 463 ndjson files had
// accumulated and ~2/3 were empty). now is injected for tests; pass
// time.Now() in production.
//
// Returns a report even on partial failure — per-file errors are
// collected so one bad unlink doesn't abort the whole sweep. A nil or
// empty logDir is a no-op (test fixtures often pass "").
func PruneAgentLogs(logDir string, maxAge time.Duration, now time.Time) AgentLogPruneReport {
	var r AgentLogPruneReport
	if logDir == "" {
		return r
	}
	dir := filepath.Join(logDir, "agents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			r.Errors = append(r.Errors, err)
		}
		return r
	}
	cutoff := now.Add(-maxAge)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".ndjson") {
			continue
		}
		r.Scanned++
		info, err := e.Info()
		if err != nil {
			r.Errors = append(r.Errors, err)
			continue
		}
		path := filepath.Join(dir, e.Name())
		switch {
		case info.Size() == 0:
			if err := os.Remove(path); err != nil {
				r.Errors = append(r.Errors, err)
				continue
			}
			r.DeletedEmpty++
		case maxAge > 0 && info.ModTime().Before(cutoff):
			if err := os.Remove(path); err != nil {
				r.Errors = append(r.Errors, err)
				continue
			}
			r.DeletedOld++
		default:
			r.Kept++
		}
	}
	return r
}

// LogPruneReport bundles PruneAgentLogs output into a structured slog line
// without forcing callers to format it themselves. Separate from the core
// prune routine so tests can exercise the math without a logger.
func LogPruneReport(logger *slog.Logger, r AgentLogPruneReport) {
	if logger == nil {
		return
	}
	logger.Info("logs.agents.prune",
		"scanned", r.Scanned,
		"deleted_old", r.DeletedOld,
		"deleted_empty", r.DeletedEmpty,
		"kept", r.Kept,
		"errors", len(r.Errors))
}
