package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/sandbox"
	"github.com/Automaat/sybra/internal/task"
)

// TestE2E_NewChecksFireThroughChecker drives the full Checker.check pipeline
// against a freshly seeded audit dir and asserts that the detectors
// (agent_retry_loop, triage_mismatch, status_bottleneck, docker_reclaimable)
// and the score rollup surface in the persisted health-report.json that the
// CLI consumes.
func TestE2E_NewChecksFireThroughChecker(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	auditDir := filepath.Join(home, "audit")
	tasksDir := filepath.Join(home, "tasks")

	logger, err := audit.NewLogger(auditDir)
	if err != nil {
		t.Fatalf("audit.NewLogger: %v", err)
	}
	t.Cleanup(func() { _ = logger.Close() })

	now := time.Now().UTC()

	seed := []audit.Event{
		// task-retry: 3 failed agent runs → checkAgentRetryLoops (critical)
		{Timestamp: now.Add(-3 * time.Hour), Type: audit.EventAgentCompleted, TaskID: "task-retry", AgentID: "a1", Data: map[string]any{"state": "error", "role": "", "cost_usd": 0.40}},
		{Timestamp: now.Add(-2 * time.Hour), Type: audit.EventAgentCompleted, TaskID: "task-retry", AgentID: "a2", Data: map[string]any{"state": "error", "role": "", "cost_usd": 0.50}},
		{Timestamp: now.Add(-1 * time.Hour), Type: audit.EventAgentCompleted, TaskID: "task-retry", AgentID: "a3", Data: map[string]any{"state": "error", "role": "", "cost_usd": 0.60}},

		// task-mismatch: triaged headless then escalated to human-required
		// → checkTriageMismatch (warning).
		{Timestamp: now.Add(-26 * time.Hour), Type: audit.EventTriageClassified, TaskID: "task-mismatch", Data: map[string]any{"mode": "headless"}},
		{Timestamp: now.Add(-2 * time.Hour), Type: audit.EventTaskStatusChanged, TaskID: "task-mismatch", Data: map[string]any{"from": "in-progress", "to": "human-required"}},

		// task-bottleneck: lingered in plan-review for 30h before moving on
		// → checkStatusBottleneck (warning, threshold 12h).
		{Timestamp: now.Add(-50 * time.Hour), Type: audit.EventTaskStatusChanged, TaskID: "task-bottleneck", Data: map[string]any{"from": "planning", "to": "plan-review"}},
		{Timestamp: now.Add(-20 * time.Hour), Type: audit.EventTaskStatusChanged, TaskID: "task-bottleneck", Data: map[string]any{"from": "plan-review", "to": "in-progress"}},
	}
	for _, e := range seed {
		if err := logger.Log(e); err != nil {
			t.Fatalf("audit log: %v", err)
		}
	}

	store, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tasks := task.NewManager(store, nil)

	silent := slog.New(slog.DiscardHandler)
	c := New(auditDir, tasks, home, silent, nil, nil)
	c.docker = func(context.Context) ([]byte, error) {
		return []byte(`{"Size":"30GiB","Reclaimable":"24GiB (80%)"}`), nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c.Run(ctx)

	report := c.LatestReport()
	if report == nil {
		t.Fatal("LatestReport returned nil")
		return
	}

	categories := map[Category]Finding{}
	for _, f := range report.Findings {
		categories[f.Category] = f
	}

	if _, ok := categories[CatAgentRetryLoop]; !ok {
		t.Errorf("expected CatAgentRetryLoop finding, got categories=%v", findingCategories(report.Findings))
	}
	if _, ok := categories[CatTriageMismatch]; !ok {
		t.Errorf("expected CatTriageMismatch finding, got categories=%v", findingCategories(report.Findings))
	}
	if _, ok := categories[CatStatusBottleneck]; !ok {
		t.Errorf("expected CatStatusBottleneck finding, got categories=%v", findingCategories(report.Findings))
	}
	if _, ok := categories[CatDockerReclaimable]; !ok {
		t.Errorf("expected CatDockerReclaimable finding, got categories=%v", findingCategories(report.Findings))
	}

	if retry, ok := categories[CatAgentRetryLoop]; ok {
		if retry.TaskID != "task-retry" {
			t.Errorf("retry-loop TaskID = %q, want task-retry", retry.TaskID)
		}
		if retry.Severity != SeverityCritical {
			t.Errorf("retry-loop severity = %q, want critical", retry.Severity)
		}
	}

	if report.Score != ScoreCritical {
		t.Errorf("Score = %q, want critical (a critical finding fired)", report.Score)
	}
	if report.Processes == nil {
		t.Fatal("Processes = nil, want non-nil summary")
	}

	// The persisted JSON is what sybra-cli health reads. Verify the score
	// and findings round-trip through the file the CLI consumes.
	data, err := os.ReadFile(filepath.Join(home, "health-report.json"))
	if err != nil {
		t.Fatalf("read persisted report: %v", err)
	}
	var persisted struct {
		Score  string `json:"score"`
		Docker *struct {
			Available        bool   `json:"available"`
			ReclaimableBytes int64  `json:"reclaimableBytes"`
			ManualCommand    string `json:"manualCommand"`
		} `json:"docker"`
		Processes *struct {
			Available bool `json:"available"`
		} `json:"processes"`
		Findings []struct {
			Category string `json:"category"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("parse persisted report: %v", err)
	}
	if persisted.Score != string(ScoreCritical) {
		t.Errorf("persisted score = %q, want critical", persisted.Score)
	}
	if persisted.Processes == nil {
		t.Fatal("persisted processes = nil, want object")
	}
	if persisted.Docker == nil {
		t.Fatal("persisted docker = nil, want object")
	}
	if !persisted.Docker.Available {
		t.Fatal("persisted docker available = false, want true")
	}
	if persisted.Docker.ReclaimableBytes != 24*(1<<30) {
		t.Errorf("persisted docker reclaimable = %d, want %d", persisted.Docker.ReclaimableBytes, 24*(1<<30))
	}
	if persisted.Docker.ManualCommand != dockerManualCommand {
		t.Errorf("persisted docker manualCommand = %q, want %q", persisted.Docker.ManualCommand, dockerManualCommand)
	}
	gotPersisted := map[string]bool{}
	for _, f := range persisted.Findings {
		gotPersisted[f.Category] = true
	}
	for _, want := range []Category{CatAgentRetryLoop, CatTriageMismatch, CatStatusBottleneck, CatDockerReclaimable} {
		if !gotPersisted[string(want)] {
			t.Errorf("persisted report missing category %q", want)
		}
	}
}

// TestE2E_GoodScoreWhenNothingFires ensures the rollup yields ScoreGood when
// the audit log only contains successful, well-behaved events.
func TestE2E_GoodScoreWhenNothingFires(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	auditDir := filepath.Join(home, "audit")
	tasksDir := filepath.Join(home, "tasks")

	logger, err := audit.NewLogger(auditDir)
	if err != nil {
		t.Fatalf("audit.NewLogger: %v", err)
	}
	t.Cleanup(func() { _ = logger.Close() })

	now := time.Now().UTC()
	clean := []audit.Event{
		{Timestamp: now.Add(-2 * time.Hour), Type: audit.EventAgentCompleted, TaskID: "task-ok", AgentID: "a1", Data: map[string]any{"state": "stopped", "role": "", "cost_usd": 0.30}},
		{Timestamp: now.Add(-1 * time.Hour), Type: audit.EventTaskStatusChanged, TaskID: "task-ok", Data: map[string]any{"from": "in-progress", "to": "done"}},
	}
	for _, e := range clean {
		if err := logger.Log(e); err != nil {
			t.Fatalf("audit log: %v", err)
		}
	}

	store, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tasks := task.NewManager(store, nil)
	silent := slog.New(slog.DiscardHandler)
	c := New(auditDir, tasks, home, silent, nil, nil)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c.Run(ctx)

	report := c.LatestReport()
	if report == nil {
		t.Fatal("LatestReport returned nil")
		return
	}
	if report.Score != ScoreGood {
		t.Errorf("Score = %q, want good (findings=%v)", report.Score, findingCategories(report.Findings))
	}
	if report.Processes == nil {
		t.Fatal("Processes = nil, want non-nil summary")
	}
}

// TestE2E_SandboxCleanupFailureSurfacesHealthFinding drives the real,
// observable workflow behind the sandbox-repair task's third acceptance
// criterion: a sandbox cleanup failure that survived sandbox.Manager's
// normalize-then-retry escalation and landed in quarantine (exercised at
// the decision-logic level, with full control over the injected failure, by
// internal/sandbox's own TestManager_RemoveContext_QuarantinesPersistentFailure)
// surfaces as exactly one deduplicated health.Finding carrying bytes
// retained, through the same Checker.check pipeline the other e2e tests
// exercise. The quarantine record is seeded on disk in the exact layout
// sandbox.Manager itself writes (dataDir/.quarantine/<taskID>.json) so this
// test reads it back through the real, unmodified QuarantinedEntries.
func TestE2E_SandboxCleanupFailureSurfacesHealthFinding(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	sandboxDir := filepath.Join(home, "sandboxes")
	mgr := sandbox.NewManager(sandboxDir, slog.New(slog.DiscardHandler))

	quarantineDir := filepath.Join(sandboxDir, ".quarantine")
	if err := os.MkdirAll(quarantineDir, 0o755); err != nil {
		t.Fatalf("mkdir quarantine dir: %v", err)
	}
	entry := sandbox.QuarantineEntry{
		TaskID:        "task-locked",
		Path:          filepath.Join(sandboxDir, "task-locked"),
		BytesRetained: 4096,
		Attempts:      3,
		LastError:     "permission denied",
		FirstFailedAt: time.Now().Add(-time.Hour).UTC(),
		LastFailedAt:  time.Now().UTC(),
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal quarantine entry: %v", err)
	}
	if err := os.WriteFile(filepath.Join(quarantineDir, "task-locked.json"), data, 0o644); err != nil {
		t.Fatalf("write quarantine entry: %v", err)
	}

	entries := mgr.QuarantinedEntries()
	if len(entries) != 1 {
		t.Fatalf("QuarantinedEntries = %+v, want exactly 1 entry", entries)
	}
	if entries[0].BytesRetained <= 0 {
		t.Fatalf("QuarantinedEntries[0].BytesRetained = %d, want > 0", entries[0].BytesRetained)
	}

	auditDir := filepath.Join(home, "audit")
	tasksDir := filepath.Join(home, "tasks")
	store, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tasks := task.NewManager(store, nil)

	c := New(auditDir, tasks, home, slog.New(slog.DiscardHandler), nil, nil)
	c.SetSandboxQuarantine(mgr.QuarantinedEntries)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c.Run(ctx)

	report := c.LatestReport()
	if report == nil {
		t.Fatal("LatestReport returned nil")
	}

	var found *Finding
	for i := range report.Findings {
		if report.Findings[i].Category == CatSandboxCleanup {
			found = &report.Findings[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected %q finding, got %v", CatSandboxCleanup, findingCategories(report.Findings))
	}
	if found.TaskID != "task-locked" {
		t.Errorf("TaskID = %q, want task-locked", found.TaskID)
	}
	wantFingerprint := string(CatSandboxCleanup) + ":task-locked"
	if found.Fingerprint != wantFingerprint {
		t.Errorf("Fingerprint = %q, want %q", found.Fingerprint, wantFingerprint)
	}
	if bytes, ok := found.Evidence["bytes_retained"].(int64); !ok || bytes <= 0 {
		t.Errorf("Evidence[bytes_retained] = %v, want positive int64", found.Evidence["bytes_retained"])
	}
	if report.Score != ScoreCritical {
		t.Errorf("Score = %q, want critical", report.Score)
	}

	// Persisted health-report.json is what sybra-cli health reads — verify
	// the finding, including bytes retained, round-trips through it.
	persistedData, err := os.ReadFile(filepath.Join(home, "health-report.json"))
	if err != nil {
		t.Fatalf("read persisted report: %v", err)
	}
	var persisted struct {
		Findings []struct {
			Category string         `json:"category"`
			TaskID   string         `json:"taskId"`
			Evidence map[string]any `json:"evidence"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(persistedData, &persisted); err != nil {
		t.Fatalf("parse persisted report: %v", err)
	}
	var persistedFound bool
	for _, f := range persisted.Findings {
		if f.Category == string(CatSandboxCleanup) && f.TaskID == "task-locked" {
			persistedFound = true
			if bytes, ok := f.Evidence["bytes_retained"].(float64); !ok || bytes <= 0 {
				t.Errorf("persisted Evidence[bytes_retained] = %v, want positive number", f.Evidence["bytes_retained"])
			}
		}
	}
	if !persistedFound {
		t.Errorf("persisted report missing %q finding for task-locked", CatSandboxCleanup)
	}

	// A second tick against the same still-quarantined entry must not
	// duplicate the finding — CleanupOrphaned-style dedup via Fingerprint.
	c.check(t.Context())
	report = c.LatestReport()
	var count int
	for _, f := range report.Findings {
		if f.Category == CatSandboxCleanup && f.TaskID == "task-locked" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("sandbox cleanup findings after second tick = %d, want 1 (deduplicated)", count)
	}
}

func TestE2E_PressureTelemetryPersistsLastReclaim(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	auditDir := filepath.Join(home, "audit")
	tasksDir := filepath.Join(home, "tasks")

	store, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tasks := task.NewManager(store, nil)

	ranAt := time.Date(2026, 7, 16, 21, 0, 0, 0, time.UTC)
	c := New(auditDir, tasks, home, slog.New(slog.DiscardHandler), nil, nil)
	c.SetPressureStatus(func() *PressureStatus {
		return &PressureStatus{
			DiskFreePct:         12.5,
			MemAvailablePct:     38.0,
			LoadPerCPU:          1.75,
			WarningDiskFreePct:  15,
			CriticalDiskFreePct: 5,
			LastReclaim: &ReclaimStatus{
				RanAt:              ranAt,
				ReclaimedBytes:     4 << 30,
				UnreclaimableBytes: 18 << 30,
				Errors:             0,
			},
		}
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c.Run(ctx)

	report := c.LatestReport()
	if report == nil || report.Pressure == nil || report.Pressure.LastReclaim == nil {
		t.Fatalf("LatestReport pressure telemetry missing: %+v", report)
	}
	if report.Pressure.DiskFreePct != 12.5 || report.Pressure.LastReclaim.ReclaimedBytes != 4<<30 {
		t.Fatalf("Pressure = %+v", *report.Pressure)
	}

	persistedData, err := os.ReadFile(filepath.Join(home, "health-report.json"))
	if err != nil {
		t.Fatalf("read persisted report: %v", err)
	}
	var persisted struct {
		Pressure *struct {
			DiskFreePct         float64 `json:"diskFreePct"`
			WarningDiskFreePct  float64 `json:"warningDiskFreePct"`
			CriticalDiskFreePct float64 `json:"criticalDiskFreePct"`
			LastReclaim         *struct {
				RanAt              string `json:"ranAt"`
				ReclaimedBytes     int64  `json:"reclaimedBytes"`
				UnreclaimableBytes int64  `json:"unreclaimableBytes"`
				Errors             int    `json:"errors"`
			} `json:"lastReclaim"`
		} `json:"pressure"`
	}
	if err := json.Unmarshal(persistedData, &persisted); err != nil {
		t.Fatalf("parse persisted report: %v", err)
	}
	if persisted.Pressure == nil || persisted.Pressure.LastReclaim == nil {
		t.Fatalf("persisted pressure telemetry missing: %+v", persisted.Pressure)
	}
	if persisted.Pressure.DiskFreePct != 12.5 || persisted.Pressure.WarningDiskFreePct != 15 || persisted.Pressure.CriticalDiskFreePct != 5 {
		t.Fatalf("persisted pressure = %+v", *persisted.Pressure)
	}
	if persisted.Pressure.LastReclaim.RanAt != ranAt.Format(time.RFC3339) {
		t.Fatalf("persisted ranAt = %q, want %q", persisted.Pressure.LastReclaim.RanAt, ranAt.Format(time.RFC3339))
	}
	if persisted.Pressure.LastReclaim.ReclaimedBytes != 4<<30 || persisted.Pressure.LastReclaim.UnreclaimableBytes != 18<<30 || persisted.Pressure.LastReclaim.Errors != 0 {
		t.Fatalf("persisted last reclaim = %+v", *persisted.Pressure.LastReclaim)
	}
}

func findingCategories(findings []Finding) []string {
	out := make([]string, len(findings))
	for i := range findings {
		out[i] = string(findings[i].Category)
	}
	return out
}
