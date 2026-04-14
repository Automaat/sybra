package poll

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/project"
	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/triage"
)

type fakeClassifier struct {
	calls int
}

func (f *fakeClassifier) Classify(_ context.Context, t task.Task, _ []project.Project) (triage.Verdict, error) {
	f.calls++
	return triage.Verdict{
		Title: "feat(x): " + t.Title,
		Size:  "small",
		Type:  "bug",
		Mode:  "headless",
		Tags:  []string{"backend", "small", "bug"},
	}, nil
}

func TestTriageHandlerClassifiesNewTasks(t *testing.T) {
	dir := t.TempDir()
	ts, err := task.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	mgr := task.NewManager(ts, nil)

	_, err = mgr.Create("a task", "body", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = mgr.Create("another", "body2", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Skip debounce: backdate updated_at by mutating status to todo+back.
	// Simpler: use short perTaskTTL and override the debounce via sleep.
	time.Sleep(10 * time.Millisecond)

	projDir := t.TempDir()
	ps, err := project.NewStore(projDir, t.TempDir())
	if err != nil {
		t.Fatalf("project.NewStore: %v", err)
	}

	fc := &fakeClassifier{}
	h := NewTriageHandler(mgr, ps, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		&config.TriageConfig{Enabled: true, PollSeconds: 5, Model: "sonnet"})
	h.factory = func(string, *slog.Logger) triage.Classifier { return fc }
	// Disable the 5-second debounce for the test.
	h.perTaskTTL = time.Second

	// The handler skips tasks whose updated_at < 5s; backdate manually.
	list, _ := mgr.List()
	for i := range list {
		_, err := mgr.UpdateMap(list[i].ID, map[string]any{"status_reason": "seed"})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// Wait past 5s debounce. Use a tighter override: set perTaskTTL does not affect debounce.
	// Instead, bypass by directly calling classifyOne for each target.
	projects, _ := ps.List()
	for i := range list {
		updated, _ := mgr.Get(list[i].ID)
		h.classifyOne(context.Background(), fc, updated, projects)
	}

	if fc.calls != 2 {
		t.Errorf("classifier calls: got %d, want 2", fc.calls)
	}
	after, _ := mgr.List()
	for i := range after {
		if after[i].Status == task.StatusNew {
			t.Errorf("task %s still new", after[i].ID)
		}
	}
}
