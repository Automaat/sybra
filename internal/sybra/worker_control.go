package sybra

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/gitexec"
	"github.com/Automaat/sybra/internal/remotehandback"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workercontrol"
)

// WorkerControlHandler exposes app's durable worker transport after database
// initialization. File-backend boards return nil because delivery state must
// survive a leader restart.
func WorkerControlHandler(a *App) http.Handler {
	if a == nil || a.workerControl == nil {
		return nil
	}
	return a.workerControl.Handler()
}

func (a *App) importRemoteHandback(ctx context.Context, runID string) error {
	handback, err := a.workerControl.LoadStagedArtifact(ctx, runID)
	if err != nil {
		return err
	}
	if err := a.workerControl.BeginArtifactImport(ctx, runID, handback.Manifest.ManifestID); err != nil {
		return err
	}
	var notify task.Task
	notifyNeeded := false
	err = a.tasks.WithMutationLock(handback.Spec.Fence.TaskID, func() error {
		t, getErr := a.tasks.Get(handback.Spec.Fence.TaskID)
		if getErr != nil {
			return getErr
		}
		target := a.worktrees.PathFor(t)
		receipted := remoteReceiptApplied(t, handback)
		original := t.Generation >= 0 && uint64(t.Generation) == handback.Spec.Fence.TaskGeneration && t.Generation == handback.Spec.Fence.WorkflowGeneration
		if t.Workflow == nil || t.Workflow.WorkflowID != handback.Spec.Fence.WorkflowID || t.Workflow.CurrentStep != handback.Spec.Fence.StepID || (!original && !receipted) {
			return a.rejectRemoteHandback(ctx, handback, remotehandback.ErrStale)
		}
		head, headErr := gitexec.Output(ctx, gitexec.Options{Dir: target}, "rev-parse", "--verify", "HEAD^{commit}")
		if headErr != nil {
			return headErr
		}
		if (original && head != handback.Spec.Workspace.BaseSHA) || (receipted && head != handback.Spec.Workspace.BaseSHA && head != handback.Manifest.Workspace.FinalSHA) {
			return a.rejectRemoteHandback(ctx, handback, remotehandback.ErrStale)
		}
		guard := func(context.Context) (executioncontract.GenerationFence, string, error) {
			current, currentErr := a.tasks.Get(handback.Spec.Fence.TaskID)
			if currentErr != nil {
				return executioncontract.GenerationFence{}, "", currentErr
			}
			original := current.Generation >= 0 && uint64(current.Generation) == handback.Spec.Fence.TaskGeneration &&
				current.Generation == handback.Spec.Fence.WorkflowGeneration
			recoveredOutputs := remoteReceiptApplied(current, handback)
			if current.Workflow == nil || current.Workflow.WorkflowID != handback.Spec.Fence.WorkflowID ||
				current.Workflow.CurrentStep != handback.Spec.Fence.StepID || (!original && !recoveredOutputs) {
				return executioncontract.GenerationFence{}, "", remotehandback.ErrStale
			}
			head, headErr := gitexec.Output(ctx, gitexec.Options{Dir: target}, "rev-parse", "--verify", "HEAD^{commit}")
			return handback.Spec.Fence, head, headErr
		}
		lock := func(_ context.Context, path string, fn func() error) error {
			return a.worktrees.WithMutationLock(path, fn)
		}
		before := func(members []executioncontract.ArtifactMember) error {
			if err := a.importRemoteOutputs(handback, members, true); err != nil {
				return err
			}
			notify, getErr = a.tasks.Get(handback.Spec.Fence.TaskID)
			notifyNeeded = getErr == nil
			return getErr
		}
		var importErr error
		if handback.Spec.Options.DiscardWorkspaceChanges {
			_, importErr = remotehandback.ValidateGitWithBeforePublish(ctx, target, handback.Spec, handback.Manifest, handback.Content, guard, lock, before)
		} else {
			_, importErr = remotehandback.ImportGitWithBeforePublish(ctx, target, handback.Spec, handback.Manifest, handback.Content, guard, lock, before)
		}
		if importErr != nil {
			if errors.Is(importErr, remotehandback.ErrStale) {
				return a.rejectRemoteHandback(ctx, handback, importErr)
			}
			return importErr
		}
		return a.workerControl.ResolveArtifact(ctx, runID, handback.Manifest.ManifestID, "imported")
	})
	if notifyNeeded {
		a.tasks.NotifyLockedMutation(notify)
	}
	return err
}

func (a *App) rejectRemoteHandback(ctx context.Context, handback workercontrol.ArtifactHandback, cause error) error {
	if err := a.workerControl.ResolveArtifact(ctx, handback.Spec.RunID, handback.Manifest.ManifestID, "rejected"); err != nil {
		return errors.Join(cause, err)
	}
	a.logger.Warn("worker.handback.rejected", "run_id", handback.Spec.RunID, "err", cause)
	// Rejection is a durable diagnostic outcome, so acknowledge the upload.
	// Returning cause would make the daemon retry a package that can no longer
	// transition out of rejected.
	return nil
}

func (a *App) importRemoteOutputs(handback workercontrol.ArtifactHandback, members []executioncontract.ArtifactMember, taskLocked bool) error {
	byPath := make(map[string]executioncontract.ArtifactEntry, len(handback.Manifest.Artifacts))
	for i := range handback.Manifest.Artifacts {
		entry := &handback.Manifest.Artifacts[i]
		byPath[string(entry.Root)+":"+entry.Path] = *entry
	}
	update := task.Update{}
	hasSidecar := false
	for i, member := range members {
		entry := byPath[string(member.Root)+":"+member.Path]
		if member.Root == executioncontract.RootSidecar {
			hasSidecar = true
			content := string(member.Content)
			switch entry.Kind {
			case "plan":
				update.Plan = task.Ptr(content)
			case "plan_contract":
				update.PlanContract = task.Ptr(content)
			case "plan_critique":
				update.PlanCritique = task.Ptr(content)
			case "plan_research":
				update.PlanResearch = task.Ptr(content)
			case "plan_decisions":
				update.PlanDecisions = task.Ptr(content)
			case "plan_brief":
				update.PlanBrief = task.Ptr(content)
			case "code_review":
				update.CodeReview = task.Ptr(content)
			case "current_test_failures":
				update.CurrentTestFailures = task.Ptr(content)
			case "acceptance_ledger":
				update.AcceptanceLedger = task.Ptr(content)
			case "spec_decision":
				update.SpecDecision = task.Ptr(content)
			default:
				return fmt.Errorf("remote handback: unsupported sidecar kind %q", entry.Kind)
			}
			continue
		}
		name := fmt.Sprintf("remote-%s-%03d%s", handback.Spec.RunID, i, filepath.Ext(entry.Path))
		name = strings.ReplaceAll(name, ":", "-")
		if _, err := a.artifacts.Put(handback.Spec.Fence.TaskID, artifact.Artifact{Name: name, Kind: artifact.KindGeneric, ProducerRole: handback.Spec.Role, StepID: handback.Spec.Fence.StepID, SourcePath: string(member.Root) + ":" + member.Path, Content: member.Content}); err != nil {
			return err
		}
	}
	if hasSidecar || taskLocked {
		apply := a.tasks.UpdateFnBy
		if taskLocked {
			apply = a.tasks.UpdateFnByWhileLocked
		}
		_, err := apply(handback.Spec.Fence.TaskID, "remotehandback.import", func(current task.Task) (task.Update, error) {
			alreadyApplied := remoteReceiptApplied(current, handback)
			if alreadyApplied {
				return task.Update{}, errRemoteOutputsAlreadyApplied
			}
			if current.Generation < 0 || uint64(current.Generation) != handback.Spec.Fence.TaskGeneration || current.Workflow == nil ||
				current.Workflow.WorkflowID != handback.Spec.Fence.WorkflowID || current.Workflow.CurrentStep != handback.Spec.Fence.StepID {
				return task.Update{}, remotehandback.ErrStale
			}
			tags := append([]string(nil), current.Tags...)
			tags = append(tags, remoteReceiptTag(handback.Manifest.ManifestID))
			for _, entry := range handback.Manifest.Artifacts {
				if entry.Root == executioncontract.RootSidecar {
					tags = append(tags, remoteSidecarReceiptTag(handback.Spec.Fence.WorkflowID, handback.Spec.Fence.StepID, handback.Spec.Fence.WorkflowGeneration+1, entry.Kind))
				}
			}
			update.Tags = &tags
			return update, nil
		})
		if errors.Is(err, errRemoteOutputsAlreadyApplied) {
			return nil
		}
		return err
	}
	return nil
}

var errRemoteOutputsAlreadyApplied = errors.New("remote handback: outputs already applied")

func remoteReceiptTag(manifestID string) string { return "remote-handback:" + manifestID }

func remoteSidecarReceiptTag(workflowID, stepID string, generation int64, kind string) string {
	return fmt.Sprintf("remote-sidecar:%s:%s:%d:%s", workflowID, stepID, generation, kind)
}

func remoteReceiptApplied(current task.Task, handback workercontrol.ArtifactHandback) bool {
	return current.Generation == handback.Spec.Fence.WorkflowGeneration+1 && slices.Contains(current.Tags, remoteReceiptTag(handback.Manifest.ManifestID))
}
