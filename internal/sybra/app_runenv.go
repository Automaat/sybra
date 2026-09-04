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
			return runenv.ProbeResult{Code: "provider_unavailable", Evidence: a.providerHealth.Reason(providerName), Owner: autonomy.FailureOwnerExternalTransient}, errors.New("provider health gate denied dispatch")
		},
		ProbeNetwork: func(_ context.Context, _ string) (runenv.ProbeResult, error) {
			if open, _ := github.AuthCircuitOpen(); open {
				snapshot := github.AuthHealthSnapshot()
				return githubAuthProbeFailure(snapshot), errors.New("GitHub auth circuit is open")
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

func githubAuthProbeFailure(snapshot github.AuthSnapshot) runenv.ProbeResult {
	owner := autonomy.FailureOwnerExternalTransient
	code := "github_auth_unavailable"
	if snapshot.State == github.AuthMisconfigured {
		owner = autonomy.FailureOwnerOperatorAuthority
		code = "github_auth_misconfigured"
	}
	return runenv.ProbeResult{Code: code, Evidence: string(snapshot.State), Owner: owner}
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
		cert, certErr := a.runenv.Certify(ctx, runenv.Request{
			TaskID: environment.TaskID, Action: "implementation.dispatch", WorkDir: environment.Dir,
			ScratchRoots: environment.ScratchRoots, Provider: environment.Provider, SandboxMode: environment.SandboxMode,
			Requirements: []autonomy.CapabilityRequirement{
				{Capability: autonomy.CapabilitySourceRead, Action: "implementation.dispatch", Scope: "task"},
				{Capability: autonomy.CapabilitySourceWrite, Action: "implementation.dispatch", Scope: "task"},
				{Capability: autonomy.CapabilityScratchWrite, Action: "implementation.dispatch", Scope: "task"},
				{Capability: autonomy.CapabilitySandboxMechanism, Action: "implementation.dispatch", Scope: "host"},
				{Capability: autonomy.CapabilityProviderCapacity, Action: "implementation.dispatch", Scope: "provider"},
			},
			ConfigVersion: environment.SandboxMode,
		})
		if certErr == nil && a.verification != nil {
			certErr = a.verification.SetCertificateForWorkspace(environment.Dir, cert.ID)
		}
		return certErr
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
	if environment.LocalCommand {
		requirements = slices.DeleteFunc(requirements, func(requirement autonomy.CapabilityRequirement) bool {
			return requirement.Capability == autonomy.CapabilityProviderCapacity
		})
	}
	mutationIdentity := ""
	if slices.ContainsFunc(requirements, func(requirement autonomy.CapabilityRequirement) bool {
		return requirement.Capability == autonomy.CapabilityTaskMutation
	}) {
		mutationIdentity, err = a.tasks.MutationTransportIdentity(t.ID)
		if err != nil {
			return err
		}
	}
	cert, err := a.runenv.Certify(ctx, runenv.Request{
		TaskID: t.ID, ProjectID: t.ProjectID, Action: action, WorkDir: environment.Dir,
		ReadRoots: environment.ReadOnlyPaths, GitRoots: preparedRunGitRoots(environment),
		ScratchRoots: environment.ScratchRoots, CloneDir: cloneDir, CloneGeneration: cloneGeneration, TaskBranch: t.Branch,
		Provider: environment.Provider, SandboxMode: environment.SandboxMode,
		SigningPolicy:        project.NormalizeSigningPolicy(a.cfg.CommitSigning()),
		TaskMutationIdentity: mutationIdentity,
		Requirements:         requirements,
		ConfigVersion:        fmt.Sprintf("%s|%s", environment.SandboxMode, a.cfg.CommitSigning()),
	})
	if err == nil && a.verification != nil {
		err = a.verification.SetCertificateForWorkspace(environment.Dir, cert.ID)
	}
	return err
}

// preparedRunGitRoots keeps checkout certification scoped to the prepared
// repository. ReadOnlyPaths also contains provider credentials, config files,
// and tool executables; those must be readable but are not Git checkouts.
func preparedRunGitRoots(environment agent.RunEnvironment) []string {
	if len(environment.GitRoots) > 0 {
		return slices.Clone(environment.GitRoots)
	}
	if strings.TrimSpace(environment.Dir) == "" {
		return nil
	}
	return []string{environment.Dir}
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

// quarantineRunEnvironment records a coalesced admission failure. Despite its
// historical name and audit event, it must never mutate tasks: an unavailable
// signer, sandbox, checkout, or similar environment dependency only prevents
// agent dispatch. The workflow remains waiting and its next retry certifies
// again, automatically continuing once the environment is healthy.
func (a *App) quarantineRunEnvironment(_ context.Context, failure runenv.CertificationError) {
	a.logger.Warn("runenv.admission.deferred", "task_id", failure.TaskID, "scope", failure.Scope, "reason_code", failure.Code)
	a.logAudit(audit.EventRunEnvironmentScopeQuarantined, failure.TaskID, "", map[string]any{"scope": failure.Scope, "reason_code": failure.Code})
}
