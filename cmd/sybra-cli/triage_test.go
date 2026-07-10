package main

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/triage"
)

type failingTriageClassifier struct{}

func (failingTriageClassifier) Classify(context.Context, task.Task, []project.Project) (triage.Verdict, error) {
	return triage.Verdict{}, errors.New("provider subprocess exited while reading stdin")
}

func TestClassifyOneFailureMarksRetryableAndPreservesTaskFields(t *testing.T) {
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	mgr := task.NewManager(store, nil)
	created, err := mgr.Create("keep original title", "keep body", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	created, err = mgr.Update(created.ID, task.Update{Tags: task.Ptr([]string{"backend", "bug"})})
	if err != nil {
		t.Fatalf("Update tags: %v", err)
	}

	_, err = classifyOne(failingTriageClassifier{}, mgr, nil, created, nil, time.Second)
	if err == nil {
		t.Fatal("classifyOne succeeded; want classifier error")
	}

	got, err := mgr.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != created.Title {
		t.Errorf("title = %q, want preserved %q", got.Title, created.Title)
	}
	if !slices.Equal(got.Tags, created.Tags) {
		t.Errorf("tags = %v, want preserved %v", got.Tags, created.Tags)
	}
	if got.Status == task.StatusHumanRequired {
		t.Fatalf("status = human-required; classifier failures must stay non-human")
	}
	if !strings.HasPrefix(got.StatusReason, triageRetryableStatusReasonPrefix) {
		t.Fatalf("status_reason = %q, want retryable prefix %q", got.StatusReason, triageRetryableStatusReasonPrefix)
	}
	if !strings.Contains(got.StatusReason, "provider subprocess exited") {
		t.Errorf("status_reason = %q, want classifier detail", got.StatusReason)
	}
}

// TestCmdTriageClassifyRefusesNonNewTask locks in the single-id `triage
// classify <id>` entry point matching the status=new filter the --all path
// and the poll-based auto-triage handler already apply. Without this guard,
// classifying an arbitrary id can reclassify a task outside the triage
// pipeline's ownership — e.g. a human-required Prompt Lab proposal whose
// gating tags/status must survive untouched (see internal/triage/apply.go's
// promptLabProposalTag).
func TestCmdTriageClassifyRefusesNonNewTask(t *testing.T) {
	dir := t.TempDir()
	store, err := task.NewStore(filepath.Join(dir, "tasks"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	mgr := task.NewManager(store, nil)
	created, err := mgr.Create("pending human decision", "body", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	humanRequired := task.StatusHumanRequired
	tags := []string{"prompt-lab-proposal", "requires-human"}
	created, err = mgr.Update(created.ID, task.Update{Status: &humanRequired, Tags: &tags})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	ps, err := project.NewStore(filepath.Join(dir, "projects"), filepath.Join(dir, "clones"))
	if err != nil {
		t.Fatalf("project.NewStore: %v", err)
	}
	cfg := config.DefaultConfig()

	code, _ := captureStdout(t, func() int {
		return cmdTriageClassify(cfg, mgr, ps, []string{created.ID}, true)
	})
	if code == 0 {
		t.Fatalf("expected non-zero exit classifying a non-new task")
	}

	got, err := mgr.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status = %s, want unchanged human-required", got.Status)
	}
	if !slices.Equal(got.Tags, tags) {
		t.Errorf("tags = %v, want unchanged %v", got.Tags, tags)
	}
}
