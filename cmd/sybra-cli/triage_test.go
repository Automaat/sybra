package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

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
