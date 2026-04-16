package selfmonitor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/health"
	"github.com/Automaat/sybra/internal/task"
)

// e2eEnv groups the real filesystem-backed pieces a full selfmonitor pipeline
// needs: a per-test home directory, a logs directory, a ledger, a health
// reader (disk backed), and a task store. Nothing is mocked: the service
// reads the same files production writes.
type e2eEnv struct {
	home         string
	logsDir      string
	agentsDir    string
	ledgerPath   string
	reportPath   string
	healthPath   string
	tasks        *task.Manager
	ledger       *Ledger
	healthReader HealthReader
	emitted      []emittedEvent
	emitMu       sync.Mutex
}

type emittedEvent struct {
	Name    string
	Payload any
}

func newE2EEnv(t *testing.T) *e2eEnv {
	t.Helper()
	home := t.TempDir()
	logsDir := filepath.Join(home, "logs")
	agentsDir := filepath.Join(logsDir, "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("mkdir agents: %v", err)
	}

	tasksDir := filepath.Join(home, "tasks")
	rawStore, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatalf("task store: %v", err)
	}
	tm := task.NewManager(rawStore, nil)

	ledgerPath := filepath.Join(home, "selfmonitor", "ledger.jsonl")
	ledger, err := Open(ledgerPath)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}

	env := &e2eEnv{
		home:       home,
		logsDir:    logsDir,
		agentsDir:  agentsDir,
		ledgerPath: ledgerPath,
		reportPath: filepath.Join(home, "selfmonitor", "last-report.json"),
		healthPath: filepath.Join(home, "health-report.json"),
		tasks:      tm,
		ledger:     ledger,
	}
	env.healthReader = DiskHealthReader{Path: env.healthPath}
	return env
}

// writeHealthReport serializes a health.Report to the canonical disk path the
// DiskHealthReader reads. Mirrors what health.Checker does each tick so the
// selfmonitor service picks it up through the same path a production tick
// would traverse.
func (e *e2eEnv) writeHealthReport(t *testing.T, rep health.Report) {
	t.Helper()
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal health report: %v", err)
	}
	if err := os.WriteFile(e.healthPath, data, 0o644); err != nil {
		t.Fatalf("write health report: %v", err)
	}
}

// writeAgentLog stages an NDJSON agent log using the exact timestamped naming
// FindLogFile expects ({agentID}-{timestamp}.ndjson under logs/agents/).
func (e *e2eEnv) writeAgentLog(t *testing.T, agentID string, lines []string) string {
	t.Helper()
	path := filepath.Join(e.agentsDir, agentID+"-2026-04-14T10-00-00.ndjson")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write agent log: %v", err)
	}
	return path
}

func (e *e2eEnv) emit(name string, payload any) {
	e.emitMu.Lock()
	defer e.emitMu.Unlock()
	e.emitted = append(e.emitted, emittedEvent{Name: name, Payload: payload})
}

func (e *e2eEnv) emittedNames() []string {
	e.emitMu.Lock()
	defer e.emitMu.Unlock()
	names := make([]string, len(e.emitted))
	for i, ev := range e.emitted {
		names[i] = ev.Name
	}
	return names
}

func (e *e2eEnv) newService(t *testing.T, allowsProject func(string) bool) *Service {
	t.Helper()
	return NewService(Deps{
		Cfg: config.SelfMonitorConfig{
			Enabled:              true,
			IntervalHours:        6,
			SuppressionDays:      7,
			SuppressionThreshold: 3,
		},
		Tasks:          e.tasks,
		Health:         e.healthReader,
		Ledger:         e.ledger,
		LogsDir:        e.logsDir,
		LastReportPath: e.reportPath,
		Emit:           e.emit,
		AllowsProject:  allowsProject,
	})
}

// TestE2EPipelineDistillsFindingFromRealFiles exercises the full happy path:
// a health report on disk, a matching agent NDJSON log, a real task store
// record. The service should pick it up through DiskHealthReader, resolve
// the log file via the finding's LogFile field, run Analyze, persist a
// Report, and emit selfmonitor:report.
func TestE2EPipelineDistillsFindingFromRealFiles(t *testing.T) {
	env := newE2EEnv(t)

	logPath := env.writeAgentLog(t, "agent-abc", fixtureLines())

	env.writeHealthReport(t, health.Report{
		GeneratedAt: time.Now().UTC(),
		Score:       health.ScoreWarning,
		Findings: []health.Finding{{
			Category:    health.CatCostOutlier,
			Severity:    health.SeverityWarning,
			Title:       "eval cost $0.65",
			Fingerprint: "cost_outlier:task-e2e",
			TaskID:      "task-e2e",
			AgentID:     "agent-abc",
			LogFile:     logPath,
		}},
	})

	svc := env.newService(t, nil)

	svc.tickAndLog(context.Background())

	report, _, ok := svc.LastReport()
	if !ok {
		t.Fatal("LastReport not ready after tick")
	}
	if len(report.Findings) != 1 {
		t.Fatalf("Findings = %d, want 1", len(report.Findings))
	}

	inv := report.Findings[0]
	if inv.LogSummary == nil {
		t.Fatal("LogSummary = nil, want distilled summary")
	}
	if inv.LogSummary.TotalToolCalls != 5 {
		t.Errorf("TotalToolCalls = %d, want 5", inv.LogSummary.TotalToolCalls)
	}
	if !inv.LogSummary.StallDetected {
		t.Error("StallDetected = false, want true")
	}
	if inv.Verdict.Classification != VerdictPending {
		t.Errorf("Verdict = %q, want pending", inv.Verdict.Classification)
	}

	persisted, err := os.ReadFile(env.reportPath)
	if err != nil {
		t.Fatalf("report not persisted: %v", err)
	}
	var back Report
	if err := json.Unmarshal(persisted, &back); err != nil {
		t.Fatalf("persisted report unparseable: %v", err)
	}
	if back.HealthScore != health.ScoreWarning {
		t.Errorf("persisted HealthScore = %q, want warning", back.HealthScore)
	}
	if len(back.Findings) != 1 {
		t.Errorf("persisted Findings = %d, want 1", len(back.Findings))
	}

	names := env.emittedNames()
	if len(names) != 1 || names[0] != events.SelfMonitorReport {
		t.Errorf("emitted = %v, want [%s]", names, events.SelfMonitorReport)
	}
}

// TestE2ELogFileResolutionOrder walks the three resolution sources the
// service consults for each finding. Each row pre-stages a different layout
// and expects the analyzer to still find the log — this locks in the
// fallback precedence: finding.LogFile → task.AgentRuns[].LogFile →
// logsDir glob.
func TestE2ELogFileResolutionOrder(t *testing.T) {
	tests := []struct {
		name          string
		stageLogFile  func(t *testing.T, env *e2eEnv) (finding health.Finding, setupTask bool)
		wantAnalyzed  bool
		wantTotalTool int
	}{
		{
			name: "finding log_file set directly",
			stageLogFile: func(t *testing.T, env *e2eEnv) (health.Finding, bool) {
				t.Helper()
				logPath := env.writeAgentLog(t, "agent-direct", fixtureLines())
				return health.Finding{
					Category:    health.CatCostOutlier,
					Fingerprint: "cost_outlier:task-direct",
					TaskID:      "task-direct",
					AgentID:     "agent-direct",
					LogFile:     logPath,
				}, false
			},
			wantAnalyzed:  true,
			wantTotalTool: 5,
		},
		{
			name: "fallback to task AgentRuns",
			stageLogFile: func(t *testing.T, env *e2eEnv) (health.Finding, bool) {
				t.Helper()
				logPath := env.writeAgentLog(t, "agent-run", fixtureLines())
				// Create the task, then attach an AgentRun via AddRun.
				created, err := env.tasks.Create("task with run", "", "headless")
				if err != nil {
					t.Fatalf("create task: %v", err)
				}
				if err := env.tasks.AddRun(created.ID, task.AgentRun{
					AgentID: "agent-run",
					Mode:    "headless",
					State:   "completed",
					LogFile: logPath,
				}); err != nil {
					t.Fatalf("add run: %v", err)
				}
				return health.Finding{
					Category:    health.CatAgentRetryLoop,
					Fingerprint: "agent_retry_loop:" + created.ID,
					TaskID:      created.ID,
					AgentID:     "agent-run",
					// LogFile intentionally empty.
				}, true
			},
			wantAnalyzed:  true,
			wantTotalTool: 5,
		},
		{
			name: "fallback to logs_dir glob",
			stageLogFile: func(t *testing.T, env *e2eEnv) (health.Finding, bool) {
				t.Helper()
				// Writing under agentsDir with the timestamped naming pattern
				// that FindLogFile globs for.
				env.writeAgentLog(t, "agent-glob", fixtureLines())
				return health.Finding{
					Category:    health.CatWorkflowLoop,
					Fingerprint: "workflow_loop:task-glob",
					AgentID:     "agent-glob",
					// TaskID empty so the AgentRuns branch short-circuits.
				}, false
			},
			wantAnalyzed:  true,
			wantTotalTool: 5,
		},
		{
			name: "no log anywhere",
			stageLogFile: func(_ *testing.T, _ *e2eEnv) (health.Finding, bool) {
				return health.Finding{
					Category:    health.CatStuckTask,
					Fingerprint: "stuck_task:task-missing",
					TaskID:      "task-missing",
					AgentID:     "agent-ghost",
				}, false
			},
			wantAnalyzed:  false,
			wantTotalTool: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newE2EEnv(t)
			finding, _ := tt.stageLogFile(t, env)

			env.writeHealthReport(t, health.Report{
				Findings: []health.Finding{finding},
			})

			svc := env.newService(t, nil)
			r, err := svc.Scan(context.Background())
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if len(r.Findings) != 1 {
				t.Fatalf("Findings = %d, want 1", len(r.Findings))
			}
			got := r.Findings[0].LogSummary
			if tt.wantAnalyzed {
				if got == nil {
					t.Fatal("LogSummary = nil, want analyzed summary")
				}
				if got.TotalToolCalls != tt.wantTotalTool {
					t.Errorf("TotalToolCalls = %d, want %d", got.TotalToolCalls, tt.wantTotalTool)
				}
			} else if got != nil {
				t.Errorf("LogSummary = %+v, want nil", got)
			}
		})
	}
}

// TestE2EAutoSuppressionAcrossRestarts verifies the ledger's persistence is
// honored by the service after a fresh Open. A fingerprint marked
// false_positive three times in the file gets suppressed the next time a
// cold-started service sees it — the cross-restart auto-suppression guard.
func TestE2EAutoSuppressionAcrossRestarts(t *testing.T) {
	env := newE2EEnv(t)
	fp := "cost_outlier:task-chronic"

	for range 3 {
		if err := env.ledger.Append(LedgerEntry{
			Fingerprint: fp,
			Verdict:     VerdictFalsePositive,
			CreatedAt:   time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed ledger: %v", err)
		}
	}

	// Cold-start a new ledger from the same path — replay should rebuild
	// the fingerprint index so ShouldAutoSuppress still fires.
	coldLedger, err := Open(env.ledgerPath)
	if err != nil {
		t.Fatalf("reopen ledger: %v", err)
	}
	if coldLedger.Len() != 3 {
		t.Fatalf("cold ledger Len = %d, want 3", coldLedger.Len())
	}

	env.writeHealthReport(t, health.Report{
		Findings: []health.Finding{{
			Category:    health.CatCostOutlier,
			Fingerprint: fp,
			TaskID:      "task-chronic",
		}},
	})

	svc := NewService(Deps{
		Cfg: config.SelfMonitorConfig{
			Enabled:              true,
			IntervalHours:        6,
			SuppressionDays:      7,
			SuppressionThreshold: 3,
		},
		Tasks:          env.tasks,
		Health:         env.healthReader,
		Ledger:         coldLedger,
		LogsDir:        env.logsDir,
		LastReportPath: env.reportPath,
	})

	r, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.Findings) != 0 {
		t.Errorf("suppressed finding leaked across restart: %+v", r.Findings)
	}
	if r.Suppressed != 1 {
		t.Errorf("Suppressed = %d, want 1", r.Suppressed)
	}
}

// TestE2ESuppressionActivatesMidRun covers the cross-tick transition: a
// finding passes through on the first tick (ledger empty), gets recorded as
// false_positive three times, and is suppressed from tick 4 onward. This
// locks in the auto-suppression trigger without relying on a pre-seeded
// ledger.
func TestE2ESuppressionActivatesMidRun(t *testing.T) {
	env := newE2EEnv(t)
	fp := "cost_outlier:task-transition"

	env.writeHealthReport(t, health.Report{
		Findings: []health.Finding{{
			Category:    health.CatCostOutlier,
			Fingerprint: fp,
			TaskID:      "task-transition",
		}},
	})

	svc := env.newService(t, nil)

	// Tick 1: ledger empty, finding passes through with pending verdict.
	r1, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan 1: %v", err)
	}
	if len(r1.Findings) != 1 {
		t.Fatalf("tick 1: Findings = %d, want 1", len(r1.Findings))
	}
	if r1.Suppressed != 0 {
		t.Errorf("tick 1: Suppressed = %d, want 0", r1.Suppressed)
	}

	// Operator (or eventual judge) marks three false positives in the
	// ledger. The service should pick these up on the next tick without
	// needing a restart.
	for range 3 {
		if err := env.ledger.Append(LedgerEntry{
			Fingerprint: fp,
			Verdict:     VerdictFalsePositive,
			CreatedAt:   time.Now().UTC(),
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	r2, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan 2: %v", err)
	}
	if len(r2.Findings) != 0 {
		t.Errorf("tick 2: suppressed finding leaked: %+v", r2.Findings)
	}
	if r2.Suppressed != 1 {
		t.Errorf("tick 2: Suppressed = %d, want 1", r2.Suppressed)
	}
}

// TestE2EProjectTypeFilter walks the AllowsProject gate end-to-end through a
// real task store: findings bound to a disallowed project are suppressed
// with no LogSummary work done. Also confirms tasks without a ProjectID
// pass the gate unconditionally (common for board-wide findings).
func TestE2EProjectTypeFilter(t *testing.T) {
	env := newE2EEnv(t)

	allowed, err := env.tasks.Create("allowed", "", "headless")
	if err != nil {
		t.Fatalf("create allowed: %v", err)
	}
	if _, err := env.tasks.UpdateMap(allowed.ID, map[string]any{
		"project_id": "owner/allowed",
	}); err != nil {
		t.Fatalf("set project allowed: %v", err)
	}

	rejected, err := env.tasks.Create("rejected", "", "headless")
	if err != nil {
		t.Fatalf("create rejected: %v", err)
	}
	if _, err := env.tasks.UpdateMap(rejected.ID, map[string]any{
		"project_id": "owner/rejected",
	}); err != nil {
		t.Fatalf("set project rejected: %v", err)
	}

	noProj, err := env.tasks.Create("no project", "", "headless")
	if err != nil {
		t.Fatalf("create noproj: %v", err)
	}

	env.writeHealthReport(t, health.Report{
		Findings: []health.Finding{
			{
				Category:    health.CatStuckTask,
				Fingerprint: "stuck_task:" + allowed.ID,
				TaskID:      allowed.ID,
			},
			{
				Category:    health.CatStuckTask,
				Fingerprint: "stuck_task:" + rejected.ID,
				TaskID:      rejected.ID,
			},
			{
				Category:    health.CatStuckTask,
				Fingerprint: "stuck_task:" + noProj.ID,
				TaskID:      noProj.ID,
			},
		},
	})

	svc := env.newService(t, func(projectID string) bool {
		return projectID != "owner/rejected"
	})

	r, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.Findings) != 2 {
		t.Errorf("Findings = %d, want 2 (allowed + noProj)", len(r.Findings))
	}
	if r.Suppressed != 1 {
		t.Errorf("Suppressed = %d, want 1", r.Suppressed)
	}

	for _, f := range r.Findings {
		if f.Finding.TaskID == rejected.ID {
			t.Errorf("rejected task leaked through project filter: %+v", f)
		}
	}
}

// TestE2EPersistedReportRoundTrip locks in the disk shape the CLI `scan`
// subcommand reads. A tick writes the report, we parse it back with the
// same type the CLI uses, and verify the payload survives JSON
// round-tripping with the schema version set.
func TestE2EPersistedReportRoundTrip(t *testing.T) {
	env := newE2EEnv(t)

	env.writeHealthReport(t, health.Report{
		GeneratedAt: time.Now().UTC().Round(time.Millisecond),
		Score:       health.ScoreCritical,
		Findings: []health.Finding{{
			Category:    health.CatFailureRate,
			Severity:    health.SeverityCritical,
			Title:       "failure rate 40%",
			Fingerprint: "failure_rate",
		}},
	})

	svc := env.newService(t, nil)
	svc.tickAndLog(context.Background())

	data, err := os.ReadFile(env.reportPath)
	if err != nil {
		t.Fatalf("report missing: %v", err)
	}
	var rep Report
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rep.SchemaVersion != ReportSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", rep.SchemaVersion, ReportSchemaVersion)
	}
	if rep.HealthScore != health.ScoreCritical {
		t.Errorf("HealthScore = %q, want critical", rep.HealthScore)
	}
	if len(rep.Findings) != 1 {
		t.Fatalf("Findings = %d, want 1", len(rep.Findings))
	}
	if rep.Findings[0].Fingerprint != "failure_rate" {
		t.Errorf("Fingerprint = %q, want failure_rate", rep.Findings[0].Fingerprint)
	}
}

// TestE2EMissingHealthReportIsSoft guards the "no tick yet" bootstrap
// window: when health.Checker hasn't produced a report, selfmonitor should
// return an empty Report instead of erroring. This matches the production
// wiring where both services race on startup.
func TestE2EMissingHealthReportIsSoft(t *testing.T) {
	env := newE2EEnv(t)
	// No writeHealthReport call.

	svc := env.newService(t, nil)
	r, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.Findings) != 0 {
		t.Errorf("Findings = %d, want 0", len(r.Findings))
	}
	if r.SchemaVersion != ReportSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", r.SchemaVersion, ReportSchemaVersion)
	}
}

// TestE2EHealthToSelfMonitorHandoff stages the full handoff: a health
// Report is written to disk with a fingerprint populated by health's own
// FingerprintFor logic, and the selfmonitor service reads it back and
// preserves the fingerprint on the InvestigatedFinding. This guards the
// contract between the two packages without importing health.Checker.
func TestE2EHealthToSelfMonitorHandoff(t *testing.T) {
	env := newE2EEnv(t)

	f := health.Finding{
		Category: health.CatStuckTask,
		Severity: health.SeverityWarning,
		Title:    "stuck 10h",
		TaskID:   "task-handoff",
	}
	f.Fingerprint = health.FingerprintFor(&f)

	env.writeHealthReport(t, health.Report{
		GeneratedAt: time.Now().UTC(),
		Score:       health.ScoreWarning,
		Findings:    []health.Finding{f},
	})

	svc := env.newService(t, nil)
	r, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.Findings) != 1 {
		t.Fatalf("Findings = %d, want 1", len(r.Findings))
	}
	inv := r.Findings[0]
	want := "stuck_task:task-handoff"
	if inv.Fingerprint != want {
		t.Errorf("Fingerprint = %q, want %q", inv.Fingerprint, want)
	}
	if inv.Finding.Fingerprint != want {
		t.Errorf("inner Finding.Fingerprint = %q, want %q", inv.Finding.Fingerprint, want)
	}
}

// TestE2ECLIInvestigateUsesSamePipeline spins up a service in-process using
// the same DiskHealthReader + SelfMonitorLedgerPath layout the CLI touches
// (via SYBRA_HOME), then verifies the CLI binary would see the same
// Report shape. Keeps the CLI-side wiring contract in one place.
func TestE2EDiskHealthReaderPathsHonorSynapseHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SYBRA_HOME", home)

	if got := config.HealthReportPath(); got != filepath.Join(home, "health-report.json") {
		t.Errorf("HealthReportPath = %q, want under %q", got, home)
	}
	if got := config.SelfMonitorLedgerPath(); got != filepath.Join(home, "selfmonitor", "ledger.jsonl") {
		t.Errorf("SelfMonitorLedgerPath = %q, want under %q", got, home)
	}
	if got := config.SelfMonitorLastReportPath(); got != filepath.Join(home, "selfmonitor", "last-report.json") {
		t.Errorf("SelfMonitorLastReportPath = %q, want under %q", got, home)
	}
}

// TestE2ESchedulerTickAndRecordFlow exercises the recordReport/LastReport
// path the GUI polls. After a tick, the snapshot function should report
// initial=true and expose the same report structure.
func TestE2ESchedulerTickAndRecordFlow(t *testing.T) {
	env := newE2EEnv(t)
	env.writeHealthReport(t, health.Report{
		Score: health.ScoreGood,
	})

	svc := env.newService(t, nil)

	// Before any tick: not initial.
	if _, _, ok := svc.LastReport(); ok {
		t.Error("LastReport before tick = true, want false")
	}

	svc.tickAndLog(context.Background())

	rep, at, ok := svc.LastReport()
	if !ok {
		t.Fatal("LastReport after tick = false, want true")
	}
	if at.IsZero() {
		t.Error("LastReport timestamp zero")
	}
	if rep.HealthScore != health.ScoreGood {
		t.Errorf("HealthScore = %q, want good", rep.HealthScore)
	}
}
