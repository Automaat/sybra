package sybra

import (
	"context"
	"errors"
	"testing"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/autonomy"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/sybra/runenv"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

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

func TestMisconfiguredGitHubAuthCertificationStaysRetryable(t *testing.T) {
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
	if failure.Permanent || !failure.Blocker.IsZero() {
		t.Fatalf("classification = %#v", failure)
	}
	if failure.Reason == "" {
		t.Fatal("retryable environment failure must explain the deferred dispatch")
	}
}

func TestSignerUnavailableCertificationStaysRetryable(t *testing.T) {
	failure := workflow.ClassifyAgentStartFailure(runenv.CertificationError{
		TaskID: "task", Code: "signer_unavailable", Capability: autonomy.CapabilitySigning,
	})
	if failure.Permanent || !failure.Blocker.IsZero() {
		t.Fatalf("classification = %#v", failure)
	}
	if failure.Reason == "" {
		t.Fatal("retryable signer failure must explain the deferred dispatch")
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
