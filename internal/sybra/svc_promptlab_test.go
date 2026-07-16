package sybra

import (
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/promptlab"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

func setupPromptLabService(t *testing.T) *PromptLabService {
	t.Helper()
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	projects, err := project.NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &PromptLabService{
		tasks:     task.NewManager(store, nil),
		artifacts: artifact.New(t.TempDir()),
		projects:  projects,
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
	tags := []string{promptlab.ProposalTag, "role:test-runner", "requires-human", "requires-human"}
	created := createProposal(t, svc, task.StatusHumanRequired, tags)
	stale := "some stale reason"
	if _, err := svc.tasks.Update(created.ID, task.Update{StatusReason: &stale}); err != nil {
		t.Fatal(err)
	}

	got, err := svc.ApproveProposal(created.ID)
	if err != nil {
		t.Fatalf("ApproveProposal: %v", err)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want in-progress", got.Status)
	}
	if got.StatusReason != "" {
		t.Fatalf("StatusReason = %q, want cleared", got.StatusReason)
	}
	if slices.Contains(got.Tags, "requires-human") {
		t.Fatalf("tags = %v, want requires-human fully removed", got.Tags)
	}
	if !slices.Contains(got.Tags, promptlab.ProposalTag) || !slices.Contains(got.Tags, "role:test-runner") {
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

// TestPromptLabService_autoApprove pins that the unattended path lands the
// task in exactly the state a human click would, differing only in the
// progress note — promptLabCoordinator relies on reusing every guard here.
func TestPromptLabService_autoApprove(t *testing.T) {
	t.Parallel()
	svc := setupPromptLabService(t)
	tags := []string{promptlab.ProposalTag, "role:test-runner", "requires-human"}
	created := createProposal(t, svc, task.StatusHumanRequired, tags)

	if err := svc.autoApprove(created.ID); err != nil {
		t.Fatalf("autoApprove: %v", err)
	}

	got, err := svc.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want in-progress", got.Status)
	}
	if slices.Contains(got.Tags, "requires-human") {
		t.Fatalf("tags = %v, want requires-human removed", got.Tags)
	}

	entries, err := svc.artifacts.ReadProgress(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Message != autoApprovedProgressNote {
		t.Fatalf("progress entries = %+v, want the auto-approved note for audit", entries)
	}
}

// TestPromptLabService_autoApprove_StaleStatusGuard pins that the unattended
// caller cannot bypass requirePendingProposal — e.g. re-approving a proposal
// a human already rejected.
func TestPromptLabService_autoApprove_StaleStatusGuard(t *testing.T) {
	t.Parallel()
	for _, status := range []task.Status{task.StatusCancelled, task.StatusDone, task.StatusInProgress, task.StatusTodo} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			svc := setupPromptLabService(t)
			created := createProposal(t, svc, status, []string{promptlab.ProposalTag})

			if err := svc.autoApprove(created.ID); err == nil {
				t.Fatal("autoApprove: want error for non-pending status")
			}
			after, err := svc.tasks.Get(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != status {
				t.Fatalf("status mutated to %q, want unchanged %q", after.Status, status)
			}
		})
	}
}

func TestPromptLabService_ApproveProposal_BackfillsSybraProject(t *testing.T) {
	t.Parallel()
	svc := setupPromptLabService(t)
	if _, err := svc.projects.CreateMeta("https://github.com/Automaat/sybra.git", project.ProjectTypePet); err != nil {
		t.Fatalf("CreateMeta: %v", err)
	}
	created := createProposal(t, svc, task.StatusHumanRequired, []string{promptlab.ProposalTag, "requires-human"})

	got, err := svc.ApproveProposal(created.ID)
	if err != nil {
		t.Fatalf("ApproveProposal: %v", err)
	}
	if got.ProjectID != promptLabProjectID {
		t.Fatalf("ProjectID = %q, want %q", got.ProjectID, promptLabProjectID)
	}
}

func TestPromptLabService_RejectProposal_WithFeedback(t *testing.T) {
	t.Parallel()
	svc := setupPromptLabService(t)
	created := createProposal(t, svc, task.StatusHumanRequired, []string{promptlab.ProposalTag, "role:test-runner", "requires-human"})

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
	created := createProposal(t, svc, task.StatusHumanRequired, []string{promptlab.ProposalTag, "requires-human"})

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
	created := createProposal(t, svc, task.StatusHumanRequired, []string{promptlab.ProposalTag, "requires-human"})

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
			created := createProposal(t, svc, status, []string{promptlab.ProposalTag, "requires-human"})

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
	created := createProposal(t, svc, task.StatusHumanRequired, []string{promptlab.ProposalTag, "requires-human"})

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
	if after.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want in-progress (unchanged by the rejected re-approve)", after.Status)
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
	created := createProposal(t, svc, task.StatusHumanRequired, []string{promptlab.ProposalTag, "requires-human"})

	got, err := svc.ApproveProposal(created.ID)
	if err != nil {
		t.Fatalf("ApproveProposal: %v, want nil error despite progress-log failure", err)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want in-progress (status change not rolled back)", got.Status)
	}
}

func TestPromptLabDispatchStarted_RequiresActiveWorkflowForAlreadyActive(t *testing.T) {
	t.Parallel()
	svc := setupPromptLabService(t)
	created := createProposal(t, svc, task.StatusInProgress, []string{promptlab.ProposalTag})

	if svc.promptLabDispatchStarted(created.ID, "", workflow.ErrWorkflowAlreadyActive) {
		t.Fatal("ErrWorkflowAlreadyActive without a task workflow must not count as started")
	}

	active := &workflow.Execution{
		WorkflowID:  "prompt-lab-author",
		CurrentStep: "author_variant",
		State:       workflow.ExecRunning,
		StartedAt:   time.Now().UTC(),
	}
	if _, err := svc.tasks.Update(created.ID, task.Update{Workflow: &active}); err != nil {
		t.Fatal(err)
	}
	if !svc.promptLabDispatchStarted(created.ID, "", workflow.ErrWorkflowAlreadyActive) {
		t.Fatal("ErrWorkflowAlreadyActive with an active task workflow should count as already started")
	}
	if !svc.promptLabDispatchStarted(created.ID, "prompt-lab-author", nil) {
		t.Fatal("matched workflow without error should count as started")
	}
	if svc.promptLabDispatchStarted(created.ID, "", nil) {
		t.Fatal("no match and no error should not count as started")
	}
	if svc.promptLabDispatchStarted(created.ID, "", errors.New("boom")) {
		t.Fatal("ordinary dispatch error should not count as started")
	}
}

func TestPromptLabDispatchStarted_WaitsForWorkflowAfterAlreadyActive(t *testing.T) {
	t.Parallel()
	svc := setupPromptLabService(t)
	created := createProposal(t, svc, task.StatusInProgress, []string{promptlab.ProposalTag})

	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(promptLabAlreadyActivePoll * 2)
		active := &workflow.Execution{
			WorkflowID:  "prompt-lab-author",
			CurrentStep: "author_variant",
			State:       workflow.ExecRunning,
			StartedAt:   time.Now().UTC(),
		}
		if _, err := svc.tasks.Update(created.ID, task.Update{Workflow: &active}); err != nil {
			t.Errorf("Update workflow: %v", err)
		}
	}()

	if !svc.promptLabDispatchStarted(created.ID, "", workflow.ErrWorkflowAlreadyActive) {
		t.Fatal("ErrWorkflowAlreadyActive should wait for a concurrent active workflow to appear")
	}
	<-done
}
