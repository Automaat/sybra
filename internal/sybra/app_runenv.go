package sybra

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/autonomy"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/sybra/runenv"
	"github.com/Automaat/sybra/internal/task"
)

func (a *App) initRunEnvironment() {
	a.runenv = runenv.New(runenv.Deps{
		ProbeSandbox: func(_ context.Context, mode string) (runenv.ProbeResult, error) {
			observation, err := agent.ProbeSandboxPosture(mode)
			return runenv.ProbeResult{Available: observation.Available, Contained: observation.Contained, Evidence: observation.Evidence}, err
		},
		ProbeProvider: func(_ context.Context, providerName string) (runenv.ProbeResult, error) {
			if a.providerHealth == nil || a.providerHealth.IsHealthy(providerName) {
				return runenv.ProbeResult{Available: true, Evidence: "provider health gate admits dispatch"}, nil
			}
			return runenv.ProbeResult{Code: "provider_unavailable", Evidence: a.providerHealth.Reason(providerName)}, errors.New("provider health gate denied dispatch")
		},
		ProbeNetwork: func(_ context.Context, _ string) (runenv.ProbeResult, error) {
			if open, _ := github.AuthCircuitOpen(); open {
				snapshot := github.AuthHealthSnapshot()
				return runenv.ProbeResult{Code: "github_auth_unavailable", Evidence: string(snapshot.State)}, errors.New("GitHub auth circuit is open")
			}
			return runenv.ProbeResult{Available: true, Evidence: "GitHub auth circuit admits requests"}, nil
		},
		ProbeTaskMutation: func(_ context.Context, taskID string) (runenv.ProbeResult, error) {
			// Deliberately validate the Manager transport without exposing its
			// store to runenv or performing a timestamp-changing no-op write.
			if a.tasks == nil {
				return runenv.ProbeResult{Code: "task_mutation_unavailable"}, errors.New("task manager unavailable")
			}
			if err := a.tasks.ProbeMutationTransport(taskID); err != nil {
				return runenv.ProbeResult{Code: "task_mutation_unavailable"}, err
			}
			return runenv.ProbeResult{Available: true, Evidence: "task manager mutation lock available"}, nil
		},
		Repair: func(ctx context.Context, req runenv.Request, failed []runenv.Observation) error {
			if strings.TrimSpace(req.CloneDir) == "" {
				return errors.New("no safe clone repair is available")
			}
			if !slices.ContainsFunc(failed, func(observation runenv.Observation) bool {
				return observation.Capability == autonomy.CapabilityObjectStore || observation.Capability == autonomy.CapabilityCheckoutHealth
			}) {
				return errors.New("no safe repair is available for this capability")
			}
			_, err := project.EnsureBareCloneHealthy(ctx, req.CloneDir, req.TaskBranch)
			return err
		},
		Quarantine: a.quarantineRunEnvironment,
		Audit: func(event string, certificate runenv.Certificate, failure *runenv.CertificationError) {
			eventType := map[string]string{
				"runenv.certified":   audit.EventRunEnvironmentCertified,
				"runenv.repair":      audit.EventRunEnvironmentRepair,
				"runenv.quarantined": audit.EventRunEnvironmentQuarantined,
			}[event]
			if eventType == "" {
				eventType = event
			}
			data := map[string]any{"certificate_id": certificate.ID, "fingerprint": certificate.Fingerprint, "action": certificate.Action, "expires_at": certificate.ExpiresAt, "repaired": certificate.Repaired}
			if failure != nil {
				data["reason_code"] = failure.Code
				data["scope"] = failure.Scope
				data["capability"] = failure.Capability
			}
			a.logAudit(eventType, certificate.TaskID, "", data)
		},
	})
	a.agentOrch.SetRunEnvironment(a.runenv)
	a.agents.SetRunEnvironmentPreflight(a.certifyPreparedRunEnvironment)
}

func (a *App) certifyPreparedRunEnvironment(ctx context.Context, environment agent.RunEnvironment) error {
	if strings.TrimSpace(environment.TaskID) == "" {
		return nil
	}
	t, err := a.tasks.Get(environment.TaskID)
	if err != nil {
		// StartK8sPocAgent is an explicit project-less smoke run. It has no
		// authoritative task or Git checkout, but its actual writable roots,
		// provider, and sandbox posture must still be certified before spawn.
		if !strings.HasPrefix(environment.TaskID, "k8s-poc-") {
			return err
		}
		_, err = a.runenv.Certify(ctx, runenv.Request{
			TaskID: environment.TaskID, Action: "implementation.dispatch", WorkDir: environment.Dir,
			ScratchRoots: environment.ScratchRoots, Provider: environment.Provider, SandboxMode: environment.SandboxMode,
			Requirements: []autonomy.CapabilityRequirement{
				{Capability: autonomy.CapabilitySourceRead, Action: "implementation.dispatch", Scope: "task"},
				{Capability: autonomy.CapabilitySourceWrite, Action: "implementation.dispatch", Scope: "task"},
				{Capability: autonomy.CapabilitySandboxMechanism, Action: "implementation.dispatch", Scope: "host"},
				{Capability: autonomy.CapabilityProviderCapacity, Action: "implementation.dispatch", Scope: "provider"},
			},
			ConfigVersion: environment.SandboxMode,
		})
		return err
	}
	cloneDir, cloneGeneration := "", ""
	if t.ProjectID != "" {
		p, projectErr := a.projects.Get(t.ProjectID)
		if projectErr != nil {
			return projectErr
		}
		cloneDir, cloneGeneration = p.ClonePath, p.CloneGeneration
	}
	action := string(environment.Role) + ".dispatch"
	if environment.Role == "" {
		action = "implementation.dispatch"
	}
	requirements := environment.Role.CapabilityRequirements(action)
	mutationIdentity := ""
	if slices.ContainsFunc(requirements, func(requirement autonomy.CapabilityRequirement) bool {
		return requirement.Capability == autonomy.CapabilityTaskMutation
	}) {
		mutationIdentity, err = a.tasks.MutationTransportIdentity(t.ID)
		if err != nil {
			return err
		}
	}
	_, err = a.runenv.Certify(ctx, runenv.Request{
		TaskID: t.ID, ProjectID: t.ProjectID, Action: action, WorkDir: environment.Dir,
		ReadRoots: environment.ReadOnlyPaths, GitRoots: environment.ReadOnlyPaths,
		ScratchRoots: environment.ScratchRoots, CloneDir: cloneDir, CloneGeneration: cloneGeneration, TaskBranch: t.Branch,
		Provider: environment.Provider, SandboxMode: environment.SandboxMode,
		SigningPolicy:        project.NormalizeSigningPolicy(a.cfg.CommitSigning()),
		TaskMutationIdentity: mutationIdentity,
		Requirements:         requirements,
		ConfigVersion:        fmt.Sprintf("%s|%s", environment.SandboxMode, a.cfg.CommitSigning()),
	})
	return err
}

// certifyStartupRunEnvironment records host posture after survivor reattach
// and stranded-verdict replay, but before the startup dispatch gate opens.
// This ordering prevents a transient host/provider observation from mutating
// recovery inputs while still guaranteeing certification before scheduling.
func (a *App) certifyStartupRunEnvironment(ctx context.Context) {
	_, err := a.runenv.Certify(ctx, runenv.Request{
		TaskID: "host", Action: "startup.host", WorkDir: a.tasksDir,
		Provider: a.cfg.Agent.Provider, SandboxMode: a.cfg.DefaultSandboxMode(),
		SigningPolicy: project.NormalizeSigningPolicy(a.cfg.CommitSigning()),
		Requirements: []autonomy.CapabilityRequirement{
			{Capability: autonomy.CapabilitySandboxMechanism, Action: "startup.host", Scope: "host"},
			{Capability: autonomy.CapabilityProviderCapacity, Action: "startup.host", Scope: "provider"},
		},
		ConfigVersion: a.cfg.DefaultSandboxMode() + "|" + a.cfg.CommitSigning(),
	})
	if err != nil {
		a.logger.Warn("runenv.startup.failed", "err", err)
	}
}

// quarantineRunEnvironment applies one generic machine-owned reason to every
// non-terminal task in the failed project scope. The runenv service coalesces
// this callback per environment fingerprint, so a broken shared clone cannot
// generate a task-update storm.
func (a *App) quarantineRunEnvironment(_ context.Context, failure runenv.CertificationError) {
	if a.tasks == nil {
		return
	}
	tasks, err := a.tasks.List()
	if err != nil {
		a.logger.Error("runenv.quarantine.list", "err", err)
		return
	}
	reasonText := fmt.Sprintf("execution environment quarantined: %s", failure.Code)
	affected := 0
	for i := range tasks {
		current := &tasks[i]
		if current.Status == task.StatusDone || current.Status == task.StatusCancelled {
			continue
		}
		if failure.Scope == "task" && current.ID != failure.TaskID {
			continue
		}
		if failure.Scope == "project" && current.ProjectID != failure.ProjectID {
			continue
		}
		if failure.Scope != "host" && failure.Scope != "provider" && failure.Scope != "project" && current.ID != failure.TaskID {
			continue
		}
		update := task.Update{StatusReason: task.Ptr(reasonText)}
		if current.Status != task.StatusHumanRequired {
			update.Status = task.Ptr(task.StatusBlocked)
			update.Escalation = task.MachineFailure("runenv."+failure.Code, reasonText)
			update.AutonomyOutcome = task.QuarantinedOutcome()
		}
		if _, updateErr := a.tasks.Update(current.ID, update); updateErr != nil {
			a.logger.Error("runenv.quarantine.update", "task_id", current.ID, "err", updateErr)
			continue
		}
		affected++
	}
	a.logAudit(audit.EventRunEnvironmentScopeQuarantined, failure.TaskID, "", map[string]any{"scope": failure.Scope, "reason_code": failure.Code, "affected_tasks": affected})
}
