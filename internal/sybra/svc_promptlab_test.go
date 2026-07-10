package sybra

import (
	"os"
	"slices"
	"testing"

	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/task"
)

func setupPromptLabService(t *testing.T) *PromptLabService {
	t.Helper()
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &PromptLabService{
		tasks:     task.NewManager(store, nil),
		artifacts: artifact.New(t.TempDir()),
	}
}

func createProposal(t *testing.T, svc *PromptLabService, status task.Status, tags []string) task.Task {
	t.Helper()
	created, err := svc.tasks.CreateFull("Tighten instructions for role test-runner", "body", task.AgentModeInteractive, task.Update{
		Status: &status,
		Tags:   &tags,
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func TestPromptLabService_ApproveProposal(t *testing.T) {
	t.Parallel()
	svc := setupPromptLabService(t)
	tags := []string{"prompt-lab-proposal", "role:test-runner", "requires-human", "requires-human"}
	created := createProposal(t, svc, task.StatusHumanRequired, tags)
	stale := "some stale reason"
	if _, err := svc.tasks.Update(created.ID, task.Update{StatusReason: &stale}); err != nil {
		t.Fatal(err)
	}

	got, err := svc.ApproveProposal(created.ID)
	if err != nil {
		t.Fatalf("ApproveProposal: %v", err)
	}
	if got.Status != task.StatusTodo {
		t.Fatalf("status = %q, want todo", got.Status)
	}
	if got.StatusReason != "" {
		t.Fatalf("StatusReason = %q, want cleared", got.StatusReason)
	}
	if slices.Contains(got.Tags, "requires-human") {
		t.Fatalf("tags = %v, want requires-human fully removed", got.Tags)
	}
	if !slices.Contains(got.Tags, "prompt-lab-proposal") || !slices.Contains(got.Tags, "role:test-runner") {
		t.Fatalf("tags = %v, want prompt-lab-proposal and role tags preserved", got.Tags)
	}

	entries, err := svc.artifacts.ReadProgress(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Kind != artifact.ProgressKindDecision {
		t.Fatalf("progress entries = %+v, want one decision entry", entries)
	}
}

func TestPromptLabService_RejectProposal_WithFeedback(t *testing.T) {
	t.Parallel()
	svc := setupPromptLabService(t)
	created := createProposal(t, svc, task.StatusHumanRequired, []string{"prompt-lab-proposal", "role:test-runner", "requires-human"})

	got, err := svc.RejectProposal(created.ID, "not worth it")
	if err != nil {
		t.Fatalf("RejectProposal: %v", err)
	}
	if got.Status != task.StatusCancelled {
		t.Fatalf("status = %q, want cancelled", got.Status)
	}
	if got.StatusReason != "not worth it" {
		t.Fatalf("StatusReason = %q, want feedback", got.StatusReason)
	}
	if slices.Contains(got.Tags, "requires-human") {
		t.Fatalf("tags = %v, want requires-human removed (symmetric with approve)", got.Tags)
	}

	entries, err := svc.artifacts.ReadProgress(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Kind != artifact.ProgressKindBlocker || entries[0].Message != "not worth it" {
		t.Fatalf("progress entries = %+v, want one blocker entry with feedback", entries)
	}
}

func TestPromptLabService_RejectProposal_EmptyFeedback(t *testing.T) {
	t.Parallel()
	svc := setupPromptLabService(t)
	created := createProposal(t, svc, task.StatusHumanRequired, []string{"prompt-lab-proposal", "requires-human"})

	got, err := svc.RejectProposal(created.ID, "")
	if err != nil {
		t.Fatalf("RejectProposal: %v", err)
	}
	if got.Status != task.StatusCancelled {
		t.Fatalf("status = %q, want cancelled", got.Status)
	}
	if got.StatusReason != rejectedNoFeedbackReason {
		t.Fatalf("StatusReason = %q, want sentinel", got.StatusReason)
	}

	entries, err := svc.artifacts.ReadProgress(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Message != rejectedNoFeedbackReason {
		t.Fatalf("progress entries = %+v, want sentinel-logged blocker entry", entries)
	}
}

func TestPromptLabService_RejectProposal_WhitespaceOnlyFeedback(t *testing.T) {
	t.Parallel()
	svc := setupPromptLabService(t)
	created := createProposal(t, svc, task.StatusHumanRequired, []string{"prompt-lab-proposal", "requires-human"})

	got, err := svc.RejectProposal(created.ID, "   \n\t  ")
	if err != nil {
		t.Fatalf("RejectProposal: %v", err)
	}
	if got.Status != task.StatusCancelled {
		t.Fatalf("status = %q, want cancelled", got.Status)
	}
	if got.StatusReason != rejectedNoFeedbackReason {
		t.Fatalf("StatusReason = %q, want sentinel for whitespace-only feedback", got.StatusReason)
	}

	entries, err := svc.artifacts.ReadProgress(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Message != rejectedNoFeedbackReason {
		t.Fatalf("progress entries = %+v, want sentinel-logged blocker entry", entries)
	}
}

func TestPromptLabService_StaleStatusGuard(t *testing.T) {
	t.Parallel()
	for _, status := range []task.Status{task.StatusTodo, task.StatusCancelled, task.StatusDone, task.StatusInProgress} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			svc := setupPromptLabService(t)
			created := createProposal(t, svc, status, []string{"prompt-lab-proposal", "requires-human"})

			if _, err := svc.ApproveProposal(created.ID); err == nil {
				t.Fatal("ApproveProposal: want error for non-pending status")
			}
			after, err := svc.tasks.Get(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != status {
				t.Fatalf("status mutated to %q, want unchanged %q", after.Status, status)
			}

			if _, err := svc.RejectProposal(created.ID, "feedback"); err == nil {
				t.Fatal("RejectProposal: want error for non-pending status")
			}
			after, err = svc.tasks.Get(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != status {
				t.Fatalf("status mutated to %q, want unchanged %q", after.Status, status)
			}
		})
	}
}

func TestPromptLabService_MissingTagGuard(t *testing.T) {
	t.Parallel()
	svc := setupPromptLabService(t)
	created := createProposal(t, svc, task.StatusHumanRequired, []string{"requires-human"})

	if _, err := svc.ApproveProposal(created.ID); err == nil {
		t.Fatal("ApproveProposal: want error for missing prompt-lab-proposal tag")
	}
	after, err := svc.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != task.StatusHumanRequired {
		t.Fatalf("status mutated to %q, want unchanged", after.Status)
	}
}

func TestPromptLabService_DoubleApprove_Idempotency(t *testing.T) {
	t.Parallel()
	svc := setupPromptLabService(t)
	created := createProposal(t, svc, task.StatusHumanRequired, []string{"prompt-lab-proposal", "requires-human"})

	if _, err := svc.ApproveProposal(created.ID); err != nil {
		t.Fatalf("first ApproveProposal: %v", err)
	}
	if _, err := svc.ApproveProposal(created.ID); err == nil {
		t.Fatal("second ApproveProposal: want error, task is no longer pending")
	}
	after, err := svc.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != task.StatusTodo {
		t.Fatalf("status = %q, want todo (unchanged by the rejected re-approve)", after.Status)
	}
}

func TestPromptLabService_AppendProgressFailure_BestEffort(t *testing.T) {
	t.Parallel()
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Point the artifact store root at a regular file so any write under it
	// fails at MkdirAll — simulating an AppendProgress failure without a
	// custom fake, since artifact.Store has no interface seam.
	badRoot, err := os.CreateTemp(t.TempDir(), "not-a-dir")
	if err != nil {
		t.Fatal(err)
	}
	badRoot.Close()

	svc := &PromptLabService{
		tasks:     task.NewManager(store, nil),
		artifacts: artifact.New(badRoot.Name()),
	}
	created := createProposal(t, svc, task.StatusHumanRequired, []string{"prompt-lab-proposal", "requires-human"})

	got, err := svc.ApproveProposal(created.ID)
	if err != nil {
		t.Fatalf("ApproveProposal: %v, want nil error despite progress-log failure", err)
	}
	if got.Status != task.StatusTodo {
		t.Fatalf("status = %q, want todo (status change not rolled back)", got.Status)
	}
}
