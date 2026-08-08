package sybra

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/gitexec"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/monitor"
	"github.com/Automaat/sybra/internal/reject"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/tasksnapshot"
	"github.com/Automaat/sybra/internal/triage"
	"github.com/Automaat/sybra/internal/umbrella"
)

// Whole-operation endpoints for sybra-cli. Umbrella expansion, triage classification, and a monitor scan each touch many tasks under locks the server owns, so exposing their pieces separately would let a client interleave with the server mid-operation.

// UmbrellaExpandDTO is the wire form of umbrella.Result.
type UmbrellaExpandDTO struct {
	UmbrellaURL string `json:"umbrellaUrl"`
	Created     int    `json:"created"`
	Skipped     int    `json:"skipped"`
	Degraded    bool   `json:"degraded"`
	ChildCount  int    `json:"childCount"`
	MaxParallel int    `json:"maxParallel"`
}

// ExpandUmbrella expands a ☂️ umbrella issue into a gated child DAG. An empty model uses the instance's configured planner.
func (s *TaskService) ExpandUmbrella(issueURL, model string) (UmbrellaExpandDTO, error) {
	if s.umbrellaExpand == nil {
		return UmbrellaExpandDTO{}, unavailableError("umbrella expansion is not enabled on this instance")
	}
	res, err := s.umbrellaExpand(issueURL, model)
	if err != nil {
		if reject.Is(err) {
			return UmbrellaExpandDTO{}, err
		}
		// The reason comes from the `gh` invocation, whose stderr can carry
		// the server's own config paths on an auth or setup failure. It is
		// logged whole and reported by pointer, the same way a failed clone
		// is, rather than forwarded verbatim.
		if s.logger != nil {
			s.logger.Error("umbrella.expand.failed", "issue", issueURL, "err", err)
		}
		return UmbrellaExpandDTO{}, validationError("expanding the umbrella issue failed; see the server log for the provider's reason")
	}
	return umbrellaResultDTO(res), nil
}

func umbrellaResultDTO(res umbrella.Result) UmbrellaExpandDTO {
	return UmbrellaExpandDTO{
		UmbrellaURL: res.UmbrellaURL,
		Created:     res.Created,
		Skipped:     res.Skipped,
		Degraded:    res.Degraded,
		ChildCount:  res.ChildCount,
		MaxParallel: res.MaxParallel,
	}
}

// TriageResultDTO pairs a classifier verdict with the task the verdict was applied to.
type TriageResultDTO struct {
	Verdict triage.Verdict `json:"verdict"`
	Task    task.Task      `json:"task"`
}

// ClassifyTask runs the triage classifier over one task and applies its verdict atomically.
func (s *TaskService) ClassifyTask(id, model string) (TriageResultDTO, error) {
	if s.tasks == nil || s.projects == nil {
		return TriageResultDTO{}, unavailableError("task store unavailable")
	}
	t, err := s.tasks.Get(id)
	if err != nil {
		return TriageResultDTO{}, boardRejectionFor("task", id, err)
	}
	// The guard belongs here, not on the caller: a client that checked the
	// status against its own board would be checking one board and mutating
	// another. Reclassifying a task another subsystem owns rewrites gating
	// tags it must not touch (see internal/triage/apply.go).
	if t.Status != task.StatusNew {
		return TriageResultDTO{}, conflictError(fmt.Sprintf(
			"task %s has status %q, not %q — triage classify only reclassifies fresh tasks",
			id, t.Status, task.StatusNew))
	}
	projects, err := s.projects.List()
	if err != nil {
		return TriageResultDTO{}, fmt.Errorf("list projects: %w", err)
	}
	cfg := s.config()
	if model == "" && cfg != nil {
		model = cfg.Triage.Model
	}
	classifier := &triage.FallbackClassifier{Model: model, Logger: slog.New(slog.DiscardHandler)}
	verdict, updated, err := triage.ClassifyAndApply(s.recoveryCtx(), classifier, s.tasks, s.audit, t, projects)
	if err != nil {
		// The stamp has to land on the board that owns the task. A client
		// stamping its own copy would leave the owning instance's task with
		// no retryable marker, and the workflow engine parks it.
		reason := triage.RetryableStatusReason(err)
		if _, markErr := s.tasks.Update(id, task.Update{StatusReason: &reason}); markErr != nil {
			return TriageResultDTO{}, errors.Join(err, fmt.Errorf("mark retryable triage failure: %w", markErr))
		}
		return TriageResultDTO{}, err
	}
	return TriageResultDTO{Verdict: verdict, Task: updated}, nil
}

// ScanMonitor runs one anomaly-detector pass and returns its report.
func (s *TaskService) ScanMonitor() (monitor.Report, error) {
	if s.monitorScan == nil {
		return monitor.Report{}, unavailableError("monitor is not running on this instance")
	}
	return s.monitorScan(context.Background())
}

// ListTaskArtifactMetas returns the artifact index for a task, untruncated and
// without content.
//
// ListTaskArtifacts serves the GUI's diagnostics panel and inlines a capped
// copy of every file, which is the wrong shape for `sybra-cli artifact list`:
// it renders sizes from the index and would report the cap instead.
func (s *TaskService) ListTaskArtifactMetas(taskID string) ([]artifact.Meta, error) {
	if s.artifacts == nil {
		return nil, unavailableError("artifact store unavailable")
	}
	metas, err := s.artifacts.List(taskID)
	if err != nil {
		return nil, boardRejectionFor("task", taskID, err)
	}
	for i := range metas {
		// SourcePath is an absolute path on the server's disk and means
		// nothing to a client, which is why the GUI's DTO clears it too.
		metas[i].SourcePath = ""
	}
	return metas, nil
}

// ReadTaskArtifact returns one artifact whole. `artifact get` writes the bytes
// to stdout for a pipeline to consume, so a truncated read would corrupt the
// output rather than shorten it.
func (s *TaskService) ReadTaskArtifact(taskID, name string) ([]byte, error) {
	if s.artifacts == nil {
		return nil, unavailableError("artifact store unavailable")
	}
	data, _, err := s.artifacts.Read(taskID, name)
	if err != nil {
		return nil, boardRejectionFor("artifact "+name+" for task", taskID, err)
	}
	return data, nil
}

// ReindexTaskArtifacts rebuilds a task's artifact index from the files on disk.
func (s *TaskService) ReindexTaskArtifacts(taskID string) error {
	if s.artifacts == nil {
		return unavailableError("artifact store unavailable")
	}
	return boardRejectionFor("task", taskID, s.artifacts.Reindex(taskID))
}

// maxSnapshotHistoryLimit bounds a caller-supplied commit count.
const maxSnapshotHistoryLimit = 10000

// TaskHistoryEntryDTO is one commit in the task snapshot history.
type TaskHistoryEntryDTO struct {
	SHA     string `json:"sha"`
	Date    string `json:"date"`
	Subject string `json:"subject"`
}

// ListTaskSnapshotHistory returns the newest commits from the snapshot repo of
// the tasks dir. The repo lives beside the board it snapshots, so a client
// reading its own would report a different board's history.
func (s *TaskService) ListTaskSnapshotHistory(limit int) ([]TaskHistoryEntryDTO, error) {
	if limit <= 0 {
		limit = 20
	}
	// git rejects a limit it cannot parse as an integer, which reaches the
	// caller as a server fault for a number they chose. Bound it here so the
	// refusal names the argument instead.
	if limit > maxSnapshotHistoryLimit {
		return nil, validationError(fmt.Sprintf("limit must be at most %d", maxSnapshotHistoryLimit))
	}
	cfg := s.config()
	if cfg == nil {
		return nil, unavailableError("task snapshot history unavailable")
	}
	gitDir := config.TaskSnapshotGitDir()
	opts := gitexec.Options{Env: tasksnapshot.BuildEnv(gitDir, cfg.TasksDir)}
	ctx := s.recoveryCtx()
	if err := gitexec.Run(ctx, opts, "rev-parse", "--git-dir"); err != nil {
		return nil, unavailableError("task snapshot history unavailable — snapshotting is disabled or has not run yet")
	}
	// An empty repo is a valid empty history, detected by HEAD resolvability
	// rather than a locale-dependent stderr string.
	hasCommits := gitexec.RunQuiet(ctx, opts, "rev-parse", "--verify", "--quiet", "HEAD") == nil
	if !hasCommits {
		return []TaskHistoryEntryDTO{}, nil
	}
	const sep = "\x1f"
	stdout, err := gitexec.RawOutput(ctx, opts, "log", "--date=iso-strict",
		"--pretty=format:%h"+sep+"%ad"+sep+"%s", fmt.Sprintf("-n%d", limit))
	if err != nil {
		return nil, err
	}
	out := []TaskHistoryEntryDTO{}
	for line := range strings.SplitSeq(strings.TrimRight(string(stdout), "\n"), "\n") {
		parts := strings.SplitN(line, sep, 3)
		if len(parts) != 3 {
			continue
		}
		out = append(out, TaskHistoryEntryDTO{SHA: parts[0], Date: parts[1], Subject: parts[2]})
	}
	return out, nil
}

// MapDuplicateIncidentsDTO reports what a duplicate mapping resolved to.
type MapDuplicateIncidentsDTO struct {
	Fingerprint string `json:"fingerprint"`
	Canonical   string `json:"canonical"`
	Duplicates  []int  `json:"duplicates"`
}

// MapDuplicateIncidents points duplicate GitHub issues at an incident's
// canonical one and records the mapping.
//
// The whole operation runs here because both ends are the server's: the
// incident ledger it reads and writes, and the issue repo from its own monitor
// config.
func (s *TaskService) MapDuplicateIncidents(fingerprint string, duplicates []int, coverage string) (MapDuplicateIncidentsDTO, error) {
	cfg := s.config()
	if cfg == nil {
		return MapDuplicateIncidentsDTO{}, unavailableError("monitor configuration unavailable")
	}
	if strings.TrimSpace(coverage) == "" || len(duplicates) == 0 {
		return MapDuplicateIncidentsDTO{}, validationError("a fingerprint, at least one duplicate issue, and a coverage summary are required")
	}
	ledger, err := monitor.NewIncidentStore(config.MonitorIncidentsDir())
	if err != nil {
		return MapDuplicateIncidentsDTO{}, err
	}
	in, ok, err := ledger.Get(fingerprint)
	if err != nil {
		return MapDuplicateIncidentsDTO{}, err
	}
	if !ok {
		return MapDuplicateIncidentsDTO{}, validationError("incident not found: " + fingerprint)
	}
	if in.IsConfidential() {
		return MapDuplicateIncidentsDTO{}, validationError("confidential incidents cannot mutate public issues")
	}
	_, canonical := github.ParseIssueURL(in.IssueURL)
	if canonical == 0 {
		return MapDuplicateIncidentsDTO{}, validationError("incident has no canonical GitHub issue")
	}
	if slices.Contains(duplicates, canonical) {
		return MapDuplicateIncidentsDTO{}, validationError(
			fmt.Sprintf("canonical issue #%d cannot be mapped as its own duplicate", canonical))
	}
	sink := monitor.NewGHIssueSink(cfg.Monitor.IssueLabel, cfg.Monitor.IssueRepo)
	if err := sink.MapDuplicateIncidents(s.recoveryCtx(), in, duplicates, coverage); err != nil {
		return MapDuplicateIncidentsDTO{}, err
	}
	if err := ledger.Link(in.Fingerprint, "", "", duplicates); err != nil {
		return MapDuplicateIncidentsDTO{}, fmt.Errorf("persist mapping: %w", err)
	}
	return MapDuplicateIncidentsDTO{Fingerprint: in.Fingerprint, Canonical: in.IssueURL, Duplicates: duplicates}, nil
}
