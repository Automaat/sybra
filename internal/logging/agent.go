package logging

import (
	"compress/gzip"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
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

// AgentLogPruneReport summarizes one EnforceAgentLogRetention run for
// callers that want to surface metrics. Kept intentionally small; totals are
// enough for the slog line and tests, and the struct is exposed so tests can
// assert the classification without parsing log output.
type AgentLogPruneReport struct {
	Scanned        int
	DeletedOld     int
	DeletedEmpty   int
	DeletedForSize int
	Compressed     int
	Protected      int
	Kept           int
	Errors         []error
}

// RetentionOptions configures EnforceAgentLogRetention's sweep of per-agent
// output under <logDir>/agents/ — the main NDJSON stream plus its
// gzip-compressed (.ndjson.gz) and stderr sidecar (.ndjson.stderr) files.
type RetentionOptions struct {
	// MaxAge deletes files older than this, in addition to the always-on
	// 0-byte sweep below. 0 disables age-based deletion.
	MaxAge time.Duration
	// GzipAfter compresses non-empty .ndjson files older than this (that
	// survived the age/empty pass) into a sibling .ndjson.gz, removing the
	// original once the .gz file is written successfully. 0 disables
	// compression. Already-compressed and .stderr files are left alone.
	GzipAfter time.Duration
	// MaxTotalBytes caps the summed size of everything left in the agents
	// dir after the age and gzip passes. When exceeded, non-protected files
	// are deleted oldest-mtime-first until the total is back under the cap
	// or only protected files remain. 0 disables size-based enforcement.
	MaxTotalBytes int64
	// ActiveLogPaths is the set of absolute .ndjson paths currently owned by
	// a live agent (see agent.Manager.ActiveLogPaths). A file whose base
	// .ndjson path appears here — including its .gz/.stderr siblings — is
	// never deleted or compressed by any pass: the owning agent process may
	// still be appending to it. Nil-safe (treated as an empty set).
	ActiveLogPaths map[string]bool
}

// EnforceAgentLogRetention prunes, compresses, and caps per-agent output
// under <logDir>/agents/ in three passes over one directory listing:
//
//  1. delete 0-byte files (always) and files older than MaxAge;
//  2. gzip-compress plain .ndjson files older than GzipAfter that survived
//     pass 1;
//  3. if the directory is still over MaxTotalBytes, delete the oldest
//     remaining files (by mtime) until it isn't, or only protected files
//     remain.
//
// A file whose base .ndjson path is in opts.ActiveLogPaths is never touched
// by any pass, regardless of age, emptiness, or the size cap — this is the
// safeguard against pruning a currently-active task's log out from under
// its running agent process. now is injected for tests; pass time.Now() in
// production. A nil or empty logDir is a no-op.
//
// Returns a report even on partial failure — per-file errors are collected
// so one bad unlink/compress doesn't abort the whole sweep.
func EnforceAgentLogRetention(logDir string, opts RetentionOptions, now time.Time) AgentLogPruneReport {
	var r AgentLogPruneReport
	if logDir == "" {
		return r
	}
	dir := filepath.Join(logDir, "agents")
	recs, err := scanAgentLogDir(dir, opts, &r)
	if err != nil {
		return r
	}

	var ageCutoff, gzipCutoff time.Time
	if opts.MaxAge > 0 {
		ageCutoff = now.Add(-opts.MaxAge)
	}
	if opts.GzipAfter > 0 {
		gzipCutoff = now.Add(-opts.GzipAfter)
	}

	kept := pruneEmptyAndOld(recs, ageCutoff, &r)
	kept = compressAged(kept, gzipCutoff, &r)
	kept = enforceSizeCap(kept, opts.MaxTotalBytes, &r)

	for _, rc := range kept {
		if !rc.protected {
			r.Kept++
		}
	}
	return r
}

// agentLogRec is one file discovered under <logDir>/agents/ during a
// EnforceAgentLogRetention sweep.
type agentLogRec struct {
	path      string
	size      int64
	modTime   time.Time
	protected bool
}

// scanAgentLogDir lists dir and builds the working set of agentLogRec the
// retention passes operate on. A missing dir is not an error (nothing to
// prune yet); any other os.ReadDir failure is recorded on r and returned so
// the caller can bail out with an empty report.
func scanAgentLogDir(dir string, opts RetentionOptions, r *AgentLogPruneReport) ([]agentLogRec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			r.Errors = append(r.Errors, err)
		}
		return nil, err
	}

	var recs []agentLogRec
	for _, e := range entries {
		if e.IsDir() || !strings.Contains(e.Name(), ".ndjson") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			r.Errors = append(r.Errors, err)
			continue
		}
		r.Scanned++
		path := filepath.Join(dir, e.Name())
		recs = append(recs, agentLogRec{
			path:      path,
			size:      info.Size(),
			modTime:   info.ModTime(),
			protected: opts.ActiveLogPaths[baseAgentLogPath(path)],
		})
	}
	return recs, nil
}

// pruneEmptyAndOld is retention pass 1: 0-byte and age-based deletion.
// Protected files are carried straight through, untouched by any pass.
func pruneEmptyAndOld(recs []agentLogRec, ageCutoff time.Time, r *AgentLogPruneReport) []agentLogRec {
	kept := make([]agentLogRec, 0, len(recs))
	for _, rc := range recs {
		if rc.protected {
			r.Protected++
			kept = append(kept, rc)
			continue
		}
		switch {
		case rc.size == 0:
			if err := os.Remove(rc.path); err != nil {
				r.Errors = append(r.Errors, err)
				kept = append(kept, rc)
				continue
			}
			r.DeletedEmpty++
		case !ageCutoff.IsZero() && rc.modTime.Before(ageCutoff):
			if err := os.Remove(rc.path); err != nil {
				r.Errors = append(r.Errors, err)
				kept = append(kept, rc)
				continue
			}
			r.DeletedOld++
		default:
			kept = append(kept, rc)
		}
	}
	return kept
}

// compressAged is retention pass 2: gzip plain .ndjson files old enough to
// compress. Skips .ndjson.gz (already compressed) and .ndjson.stderr
// sidecars. A zero gzipCutoff (GzipAfter disabled) is a no-op.
func compressAged(kept []agentLogRec, gzipCutoff time.Time, r *AgentLogPruneReport) []agentLogRec {
	if gzipCutoff.IsZero() {
		return kept
	}
	for i, rc := range kept {
		if rc.protected || !strings.HasSuffix(rc.path, ".ndjson") {
			continue
		}
		if !rc.modTime.Before(gzipCutoff) {
			continue
		}
		gzPath, size, err := gzipAndRemove(rc.path)
		if err != nil {
			r.Errors = append(r.Errors, err)
			continue
		}
		kept[i].path = gzPath
		kept[i].size = size
		r.Compressed++
	}
	return kept
}

// enforceSizeCap is retention pass 3: enforce the total-size cap, oldest
// non-protected file first. A zero maxTotalBytes (disabled) is a no-op.
func enforceSizeCap(kept []agentLogRec, maxTotalBytes int64, r *AgentLogPruneReport) []agentLogRec {
	if maxTotalBytes <= 0 {
		return kept
	}
	var total int64
	for _, rc := range kept {
		total += rc.size
	}
	if total <= maxTotalBytes {
		return kept
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].modTime.Before(kept[j].modTime) })
	survivors := make([]agentLogRec, 0, len(kept))
	for _, rc := range kept {
		if total > maxTotalBytes && !rc.protected {
			if err := os.Remove(rc.path); err != nil {
				r.Errors = append(r.Errors, err)
				survivors = append(survivors, rc)
				continue
			}
			r.DeletedForSize++
			total -= rc.size
			continue
		}
		survivors = append(survivors, rc)
	}
	return survivors
}

// PruneAgentLogs removes per-agent NDJSON files under <logDir>/agents/
// older than maxAge, plus all 0-byte files regardless of age. Kept as a
// narrow age/empty-only entry point for callers (and tests) that don't need
// gzip or size-cap enforcement — internally a thin call into
// EnforceAgentLogRetention. now is injected for tests; pass time.Now() in
// production. A nil or empty logDir is a no-op.
func PruneAgentLogs(logDir string, maxAge time.Duration, now time.Time) AgentLogPruneReport {
	return EnforceAgentLogRetention(logDir, RetentionOptions{MaxAge: maxAge}, now)
}

// baseAgentLogPath strips the .gz/.stderr sibling suffixes back down to the
// owning .ndjson path, so a gzip-compressed or stderr-sidecar file can be
// matched against RetentionOptions.ActiveLogPaths (which tracks the plain
// .ndjson path an agent was given at spawn).
func baseAgentLogPath(path string) string {
	switch {
	case strings.HasSuffix(path, ".ndjson.gz"):
		return strings.TrimSuffix(path, ".gz")
	case strings.HasSuffix(path, ".ndjson.stderr"):
		return strings.TrimSuffix(path, ".stderr")
	default:
		return path
	}
}

// gzipAndRemove compresses path into a sibling "<path>.gz" and removes the
// original on success, returning the new path and its size. A partial .gz
// file left by a failed copy/close is cleaned up before returning the error;
// the original is left in place on any failure.
func gzipAndRemove(path string) (dst string, size int64, err error) {
	src, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer src.Close()
	srcInfo, err := src.Stat()
	if err != nil {
		return "", 0, err
	}

	gzPath := path + ".gz"
	gzFile, err := os.OpenFile(gzPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", 0, err
	}
	gw := gzip.NewWriter(gzFile)
	_, copyErr := io.Copy(gw, src)
	closeErr := gw.Close()
	if fileCloseErr := gzFile.Close(); closeErr == nil {
		closeErr = fileCloseErr
	}
	if copyErr != nil || closeErr != nil {
		if rmErr := os.Remove(gzPath); rmErr != nil {
			slog.Warn("logs.agents.gzip_cleanup_failed", "path", gzPath, "error", rmErr)
		}
		if copyErr != nil {
			return "", 0, copyErr
		}
		return "", 0, closeErr
	}
	if err := os.Chtimes(gzPath, srcInfo.ModTime(), srcInfo.ModTime()); err != nil {
		if rmErr := os.Remove(gzPath); rmErr != nil {
			slog.Warn("logs.agents.gzip_cleanup_failed", "path", gzPath, "error", rmErr)
		}
		return "", 0, err
	}

	if err := os.Remove(path); err != nil {
		return "", 0, err
	}
	info, err := os.Stat(gzPath)
	if err != nil {
		return gzPath, 0, nil
	}
	return gzPath, info.Size(), nil
}

// LogPruneReport bundles an EnforceAgentLogRetention/PruneAgentLogs report
// into a structured slog line without forcing callers to format it
// themselves. Separate from the sweep itself so tests can exercise the math
// without a logger.
func LogPruneReport(logger *slog.Logger, r AgentLogPruneReport) {
	if logger == nil {
		return
	}
	logger.Info("logs.agents.prune",
		"scanned", r.Scanned,
		"deleted_old", r.DeletedOld,
		"deleted_empty", r.DeletedEmpty,
		"deleted_for_size", r.DeletedForSize,
		"compressed", r.Compressed,
		"protected", r.Protected,
		"kept", r.Kept,
		"errors", len(r.Errors))
}
