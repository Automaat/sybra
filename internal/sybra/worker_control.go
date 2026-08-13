package sybra

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
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
	t, err := a.tasks.Get(handback.Spec.Fence.TaskID)
	if err != nil {
		return a.rejectRemoteHandback(ctx, handback, err)
	}
	target := a.worktrees.PathFor(t)
	guard := func(context.Context) (executioncontract.GenerationFence, string, error) {
		current, getErr := a.tasks.Get(handback.Spec.Fence.TaskID)
		if getErr != nil {
			return executioncontract.GenerationFence{}, "", getErr
		}
		if current.Generation < 0 || uint64(current.Generation) != handback.Spec.Fence.TaskGeneration || current.Workflow == nil ||
			current.Workflow.WorkflowID != handback.Spec.Fence.WorkflowID || current.Workflow.CurrentStep != handback.Spec.Fence.StepID ||
			int64(current.Generation) != handback.Spec.Fence.WorkflowGeneration {
			return executioncontract.GenerationFence{}, "", remotehandback.ErrStale
		}
		head, headErr := gitexec.Output(ctx, gitexec.Options{Dir: target}, "rev-parse", "--verify", "HEAD^{commit}")
		return handback.Spec.Fence, head, headErr
	}
	lock := func(_ context.Context, path string, fn func() error) error {
		return a.worktrees.WithMutationLock(path, fn)
	}
	members, err := remotehandback.ImportGit(ctx, target, handback.Spec, handback.Manifest, handback.Content, guard, lock)
	if err != nil {
		return a.rejectRemoteHandback(ctx, handback, err)
	}
	if err := a.importRemoteOutputs(handback, members); err != nil {
		return a.rejectRemoteHandback(ctx, handback, err)
	}
	return a.workerControl.ResolveArtifact(ctx, runID, handback.Manifest.ManifestID, "imported")
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

func (a *App) importRemoteOutputs(handback workercontrol.ArtifactHandback, members []executioncontract.ArtifactMember) error {
	byPath := make(map[string]executioncontract.ArtifactEntry, len(handback.Manifest.Artifacts))
	for _, entry := range handback.Manifest.Artifacts {
		byPath[string(entry.Root)+":"+entry.Path] = entry
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
			case "plan_decision":
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
	if hasSidecar {
		_, err := a.tasks.UpdateFnBy(handback.Spec.Fence.TaskID, "remotehandback.import", func(current task.Task) (task.Update, error) {
			if current.Generation < 0 || uint64(current.Generation) != handback.Spec.Fence.TaskGeneration {
				return task.Update{}, remotehandback.ErrStale
			}
			return update, nil
		})
		return err
	}
	return nil
}
