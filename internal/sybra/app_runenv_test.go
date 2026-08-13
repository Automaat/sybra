package sybra

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/autonomy"
	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/sybra/runenv"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

func TestPreparedRunGitRootsExcludeAuxiliaryReadPaths(t *testing.T) {
	checkout := t.TempDir()
	environment := agent.RunEnvironment{
		Dir:           checkout,
		ReadOnlyPaths: []string{checkout, filepath.Join(t.TempDir(), "gitconfig"), "/usr/bin/gh"},
	}
	got := preparedRunGitRoots(environment)
	if len(got) != 1 || got[0] != checkout {
		t.Fatalf("preparedRunGitRoots() = %v, want only checkout %q", got, checkout)
	}
	if got := preparedRunGitRoots(agent.RunEnvironment{}); got != nil {
		t.Fatalf("preparedRunGitRoots(empty) = %v, want nil", got)
	}
	candidates := []string{"/tmp/attempt-a", "/tmp/attempt-b"}
	got = preparedRunGitRoots(agent.RunEnvironment{Dir: t.TempDir(), ReadOnlyPaths: candidates, GitRoots: candidates})
	if !slices.Equal(got, candidates) {
		t.Fatalf("preparedRunGitRoots(candidates) = %v, want %v", got, candidates)
	}
}

func TestAgentRunEnvironmentFailedReadsExitAndStreamDiagnostics(t *testing.T) {
	tests := []struct {
		name  string
		agent func() *agent.Agent
		want  bool
	}{
		{name: "clean", agent: func() *agent.Agent { return &agent.Agent{} }, want: false},
		{name: "exit error", agent: func() *agent.Agent {
			ag := &agent.Agent{}
			ag.SetExitErr(errors.New("fatal: read-only file system"))
			return ag
		}, want: true},
		{name: "terminal content", agent: func() *agent.Agent {
			ag := &agent.Agent{}
			ag.AppendOutput(agent.StreamEvent{Type: "result", Content: "commit failed: gpg failed to sign the data"})
			return ag
		}, want: true},
		{name: "assistant prose is not a diagnostic", agent: func() *agent.Agent {
			ag := &agent.Agent{}
			ag.AppendOutput(agent.StreamEvent{Type: "assistant", Content: "I fixed the read-only file system handling"})
			return ag
		}, want: false},
		{name: "structured error is a diagnostic", agent: func() *agent.Agent {
			ag := &agent.Agent{}
			ag.AppendOutput(agent.StreamEvent{Type: "system", ErrorType: "read-only file system"})
			return ag
		}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := agentRunEnvironmentFailed(tt.agent()); got != tt.want {
				t.Fatalf("agentRunEnvironmentFailed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGitHubAuthProbeFailurePreservesTransientAndAuthorityOwnership(t *testing.T) {
	tests := []struct {
		state github.AuthState
		owner autonomy.FailureOwner
		code  string
	}{
		{state: github.AuthUnavailable, owner: autonomy.FailureOwnerExternalTransient, code: "github_auth_unavailable"},
		{state: github.AuthRateLimited, owner: autonomy.FailureOwnerExternalTransient, code: "github_auth_unavailable"},
		{state: github.AuthMisconfigured, owner: autonomy.FailureOwnerOperatorAuthority, code: "github_auth_misconfigured"},
	}
	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			got := githubAuthProbeFailure(github.AuthSnapshot{State: tt.state})
			if got.Owner != tt.owner || got.Code != tt.code {
				t.Fatalf("probe failure = %+v, want owner=%q code=%q", got, tt.owner, tt.code)
			}
		})
	}
}

func TestMisconfiguredGitHubAuthCertificationRequiresCredentialAuthority(t *testing.T) {
	// Misconfigured (as opposed to merely unavailable/rate-limited) GitHub auth
	// is operator-authority owned: it does not self-heal, so it must escalate
	// instead of retrying forever with no human-visible signal — the
	// board_stalled failure mode this test guards against.
	service := runenv.New(runenv.Deps{ProbeNetwork: func(context.Context, string) (runenv.ProbeResult, error) {
		return githubAuthProbeFailure(github.AuthSnapshot{State: github.AuthMisconfigured}), errors.New("GitHub auth circuit is open")
	}})
	_, err := service.Certify(context.Background(), runenv.Request{
		TaskID: "review-task", ProjectID: "owner/repo", Action: "review.dispatch", WorkDir: t.TempDir(),
		Requirements: []autonomy.CapabilityRequirement{{Capability: autonomy.CapabilityNetworkGitHub, Action: "review.dispatch", Scope: "project"}},
	})
	if err == nil {
		t.Fatal("Certify succeeded with misconfigured GitHub auth")
	}
	failure := workflow.ClassifyAgentStartFailure(err)
	if !failure.Permanent || failure.Blocker.Kind != blocker.KindCredentialRequired || !blocker.AllowsHumanRequired(failure.Blocker.Kind) {
		t.Fatalf("classification = %#v", failure)
	}
}

func TestSignerUnavailableCertificationRequiresRepair(t *testing.T) {
	// The signing probe never sets an Owner (see runenv.go's
	// autonomy.CapabilitySigning case), so this is neither external-transient
	// nor operator-authority — it falls to the generic machine-owned branch,
	// same as checkout/object-store/sandbox/task-mutation failures. A
	// misconfigured or missing signer does not self-heal, so it must escalate
	// instead of retrying forever with no human-visible signal.
	failure := workflow.ClassifyAgentStartFailure(runenv.CertificationError{
		TaskID: "task", Code: "signer_unavailable", Capability: autonomy.CapabilitySigning,
	})
	if !failure.Permanent || failure.Blocker.Kind != blocker.KindRunEnvironment || blocker.AllowsHumanRequired(failure.Blocker.Kind) {
		t.Fatalf("classification = %#v", failure)
	}
	if failure.Blocker.Code != "signer_unavailable" {
		t.Fatalf("blocker code = %q", failure.Blocker.Code)
	}
}

func TestQuarantineRunEnvironmentDoesNotMoveTask(t *testing.T) {
	a := setupApp(t)
	created, err := a.tasks.Create("quarantine me", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.tasks.Update(created.ID, task.Update{Status: task.Ptr(task.StatusInProgress)}); err != nil {
		t.Fatal(err)
	}
	a.quarantineRunEnvironment(context.Background(), runenv.CertificationError{
		TaskID: created.ID, Action: "implementation.dispatch", Scope: "task", Code: "checkout_unhealthy", Capability: autonomy.CapabilityCheckoutHealth,
	})
	got, err := a.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusInProgress || got.AutonomyOutcome != "" {
		t.Fatalf("environment admission changed task status/outcome = %q/%q", got.Status, got.AutonomyOutcome)
	}
}
