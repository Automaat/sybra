package sybra

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/umbrella"
)

// newUmbrellaRecoveryApp builds an App wired for umbrella recovery
// scheduling tests: a real task store, a real (empty) project store, and
// umbrella recovery enabled by default. No planner call is ever live in
// these tests — recoverDegradedUmbrellas' scheduling/eligibility logic never
// needs one, and any test that reaches the actual recovery attempt overrides
// umbrellaRecoverFn.
func newUmbrellaRecoveryApp(t *testing.T) (app *App, tasks *task.Manager, projects *project.Store, projectsDir string) {
	t.Helper()
	dir := t.TempDir()
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tasks = task.NewManager(store, task.EmitterFunc(func(string, any) {}))
	projectsDir = t.TempDir()
	projects, err = project.NewStore(projectsDir, t.TempDir())
	if err != nil {
		t.Fatalf("project.NewStore: %v", err)
	}
	cfg := &config.Config{Umbrella: config.UmbrellaConfig{Enabled: true}}
	app = &App{
		tasks:                    tasks,
		projects:                 projects,
		cfg:                      cfg,
		logger:                   slog.New(slog.DiscardHandler),
		ctx:                      context.Background(),
		umbrellaRecoveryInFlight: make(map[string]bool),
	}
	return app, tasks, projects, projectsDir
}

func writeProjectYAML(t *testing.T, dir, id string, ptype project.ProjectType) {
	t.Helper()
	safe := strings.ReplaceAll(id, "/", "--")
	path := filepath.Join(dir, safe+".yaml")
	content := "id: " + id + "\ntype: " + string(ptype) + "\nowner: stub\nrepo: stub\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write project YAML: %v", err)
	}
}

// mkDegradedTracker creates an umbrella tracker carrying FallbackTag (plus
// any extraTags) in the given status/reason, optionally attached to a
// project.
func mkDegradedTracker(t *testing.T, tasks *task.Manager, umb string, status task.Status, reason, projectID string, extraTags ...string) task.Task {
	t.Helper()
	tags := append([]string{"umbrella", umbrella.MaxParallelTag(3), umbrella.FallbackTag}, extraTags...)
	tk, err := tasks.CreateFull("umbrella", "", task.AgentModeHeadless, task.Update{
		Issue:        task.Ptr(umb),
		TaskType:     task.Ptr(task.TaskTypeUmbrella),
		ProjectID:    task.Ptr(projectID),
		Status:       task.Ptr(status),
		StatusReason: task.Ptr(reason),
		Tags:         task.Ptr(tags),
	})
	if err != nil {
		t.Fatalf("create degraded tracker: %v", err)
	}
	return tk
}

// countingRecoverFn returns a stub umbrellaRecoverFn that records every call
// and blocks until release() is called, so tests can deterministically
// observe the in-flight window before letting the goroutine finish.
func countingRecoverFn() (fn func(task.Task), calls func() []string, release func()) {
	var mu sync.Mutex
	var got []string
	block := make(chan struct{})
	var once sync.Once
	fn = func(tr task.Task) {
		mu.Lock()
		got = append(got, tr.ID)
		mu.Unlock()
		<-block
	}
	calls = func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
	release = func() { once.Do(func() { close(block) }) }
	return fn, calls, release
}

func TestRecoverDegradedUmbrellas_DisabledSkipsPlannerInvocation(t *testing.T) {
	t.Parallel()
	app, tasks, _, _ := newUmbrellaRecoveryApp(t)
	app.cfg.Umbrella.Enabled = false
	mkDegradedTracker(t, tasks, "https://github.com/o/r/issues/1", task.StatusInProgress, "", "")
	fn, calls, release := countingRecoverFn()
	app.umbrellaRecoverFn = fn
	defer release()

	app.recoverDegradedUmbrellas()

	if got := calls(); len(got) != 0 {
		t.Fatalf("recover calls = %v, want none while umbrella.enabled=false", got)
	}
}

func TestRecoverDegradedUmbrellas_DuplicateTrackersSkipped(t *testing.T) {
	t.Parallel()
	app, tasks, _, _ := newUmbrellaRecoveryApp(t)
	const umb = "https://github.com/o/r/issues/1"
	mkDegradedTracker(t, tasks, umb, task.StatusInProgress, "", "")
	mkDegradedTracker(t, tasks, umb, task.StatusInProgress, "", "") // duplicate tracker for the same ref
	fn, calls, release := countingRecoverFn()
	app.umbrellaRecoverFn = fn
	defer release()

	app.recoverDegradedUmbrellas()

	if got := calls(); len(got) != 0 {
		t.Fatalf("recover calls = %v, want none for a duplicate tracker group", got)
	}
}

func TestRecoverDegradedUmbrellas_EligibleTrackerScheduledAndCleared(t *testing.T) {
	t.Parallel()
	app, tasks, _, _ := newUmbrellaRecoveryApp(t)
	tracker := mkDegradedTracker(t, tasks, "https://github.com/o/r/issues/1", task.StatusInProgress, "", "")
	fn, calls, release := countingRecoverFn()
	app.umbrellaRecoverFn = fn

	app.recoverDegradedUmbrellas()

	// Marked in-flight synchronously, before the goroutine's recoverFn call
	// necessarily even started.
	if !app.umbrellaRecoveryInFlightSnapshot()[umbrella.NormalizeIssueRef(tracker.Issue)] {
		t.Fatal("tracker ref not marked in-flight after scheduling")
	}
	release()
	app.wg.Wait()

	if got := calls(); len(got) != 1 || got[0] != tracker.ID {
		t.Fatalf("recover calls = %v, want exactly [%s]", got, tracker.ID)
	}
	if app.umbrellaRecoveryInFlightSnapshot()[umbrella.NormalizeIssueRef(tracker.Issue)] {
		t.Fatal("in-flight marker not cleared after recovery finished")
	}
}

func TestRecoverDegradedUmbrellas_AlreadyInFlightSkipsScheduling(t *testing.T) {
	t.Parallel()
	app, tasks, _, _ := newUmbrellaRecoveryApp(t)
	tracker := mkDegradedTracker(t, tasks, "https://github.com/o/r/issues/1", task.StatusInProgress, "", "")
	app.umbrellaRecoveryInFlight[umbrella.NormalizeIssueRef(tracker.Issue)] = true
	fn, calls, release := countingRecoverFn()
	app.umbrellaRecoverFn = fn
	defer release()

	app.recoverDegradedUmbrellas()

	if got := calls(); len(got) != 0 {
		t.Fatalf("recover calls = %v, want none for an already in-flight ref", got)
	}
}

func TestRecoverDegradedUmbrellas_CapsConcurrentRecoveries(t *testing.T) {
	t.Parallel()
	app, tasks, _, _ := newUmbrellaRecoveryApp(t)
	for i := 1; i <= maxConcurrentUmbrellaRecoveries+1; i++ {
		mkDegradedTracker(t, tasks, "https://github.com/o/r/issues/"+strconv.Itoa(i), task.StatusInProgress, "", "")
	}
	fn, _, release := countingRecoverFn()
	app.umbrellaRecoverFn = fn

	app.recoverDegradedUmbrellas()

	if got := len(app.umbrellaRecoveryInFlightSnapshot()); got != maxConcurrentUmbrellaRecoveries {
		t.Fatalf("in-flight recoveries = %d, want cap %d", got, maxConcurrentUmbrellaRecoveries)
	}
	release()
	app.wg.Wait()
}

func TestUmbrellaRecoveryEligible(t *testing.T) {
	t.Parallel()
	now := time.Now()
	base := task.Task{ID: "t1", Issue: "https://github.com/o/r/issues/1", Status: task.StatusInProgress}

	tests := []struct {
		name string
		mod  func(task.Task) task.Task
		want bool
	}{
		{"eligible baseline", func(tr task.Task) task.Task { return tr }, true},
		{"invalid issue ref", func(tr task.Task) task.Task { tr.Issue = "not-a-ref"; return tr }, false},
		{"done tracker", func(tr task.Task) task.Task { tr.Status = task.StatusDone; return tr }, false},
		{"cancelled tracker", func(tr task.Task) task.Task { tr.Status = task.StatusCancelled; return tr }, false},
		{"human-required unrelated reason", func(tr task.Task) task.Task {
			tr.Status, tr.StatusReason = task.StatusHumanRequired, "umbrella dependency cycle detected"
			return tr
		}, false},
		{"human-required empty reason", func(tr task.Task) task.Task {
			tr.Status = task.StatusHumanRequired
			return tr
		}, true},
		{"human-required recovery-owned reason", func(tr task.Task) task.Task {
			tr.Status, tr.StatusReason = task.StatusHumanRequired, umbrella.RecoveryFailureReasonPrefix+"3): boom"
			return tr
		}, true},
		{"blocked unrelated reason", func(tr task.Task) task.Task {
			tr.Status, tr.StatusReason = task.StatusBlocked, "blocked awaiting operator action"
			return tr
		}, false},
		{"exhausted", func(tr task.Task) task.Task {
			tr.Tags = []string{umbrella.RecoverExhaustedTag}
			return tr
		}, false},
		{"cooling down", func(tr task.Task) task.Task {
			tr.Tags = []string{umbrella.RecoverAfterTag(now.Add(time.Hour))}
			return tr
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			app := &App{logger: slog.New(slog.DiscardHandler)}
			tr := tt.mod(base)
			if got := app.umbrellaRecoveryEligible(tr, now); got != tt.want {
				t.Errorf("umbrellaRecoveryEligible(%+v) = %v, want %v", tr, got, tt.want)
			}
		})
	}
}

func TestUmbrellaRecoveryEligible_ProjectFiltering(t *testing.T) {
	t.Parallel()
	app, _, _, projectsDir := newUmbrellaRecoveryApp(t)
	writeProjectYAML(t, projectsDir, "o/allowed", project.ProjectTypePet)
	writeProjectYAML(t, projectsDir, "o/blocked", project.ProjectTypeWork)
	app.cfg.ProjectTypes = []string{"pet"}

	base := task.Task{ID: "t1", Issue: "https://github.com/o/r/issues/1", Status: task.StatusInProgress}

	tests := []struct {
		name      string
		projectID string
		want      bool
	}{
		{"no project id is not filtered", "", true},
		{"allowed project type", "o/allowed", true},
		{"disallowed project type", "o/blocked", false},
		{"missing project record", "o/missing", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tr := base
			tr.ProjectID = tt.projectID
			if got := app.umbrellaRecoveryEligible(tr, time.Now()); got != tt.want {
				t.Errorf("umbrellaRecoveryEligible(projectID=%q) = %v, want %v", tt.projectID, got, tt.want)
			}
		})
	}
}

func TestUmbrellaRecoveryExpandOptions(t *testing.T) {
	t.Parallel()
	app, _, _, _ := newUmbrellaRecoveryApp(t)

	app.cfg.Umbrella.Ground = false
	if got := app.umbrellaRecoveryExpandOptions(); len(got) != 0 {
		t.Fatalf("ground disabled: opts = %v, want none", got)
	}

	app.cfg.Umbrella.Ground = true
	app.cfg.Umbrella.GroundMinSubIssues = 4
	if got := app.umbrellaRecoveryExpandOptions(); len(got) != 1 {
		t.Fatalf("ground enabled: opts = %v, want exactly one grounder option", got)
	}
}
