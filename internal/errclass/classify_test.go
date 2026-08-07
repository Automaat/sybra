package errclass

import (
	"errors"
	"testing"
)

func TestClassifyPolicies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		policy Policy
		text   string
		want   Class
	}{
		{"poller retries plain 500", GitHubPollerRetryBiased, "gh: HTTP 500", Transient},
		{"poller shares workflow 401", GitHubPollerRetryBiased, "401 Unauthorized", Auth},
		{"poller deadline", GitHubPollerRetryBiased, "context deadline exceeded", Transient},
		{"command rejects plain 500", GHCommandEscalationBiased, "gh: HTTP 500", Permanent},
		{"command retries gateway", GHCommandEscalationBiased, "gh: HTTP 502", Transient},
		{"command exposes rate limit", GHCommandEscalationBiased, "API rate limit exceeded", RateLimited},
		{"monitor knows generic rate limit", MonitorCooldownBiased, "rate limit exceeded", RateLimited},
		{"monitor knows shared wall", MonitorCooldownBiased, "github rate-limit wall", RateLimited},
		{"token mint bare rate limit", GitHubTokenMintCooldownBiased, "request forbidden: rate limit", RateLimited},
		{"git deadline stays unknown", GitTransportEscalationBiased, "context deadline exceeded", Unknown},
		{"git timeout is transient", GitTransportEscalationBiased, "connection timed out", Transient},
		{"permanent tls is not retried", GitTransportEscalationBiased, "tls: first record does not look like a TLS handshake", Permanent},
		{"workflow prose deadline", WorkflowProseRetryBiased, "context deadline exceeded", Transient},
		{"workflow github rate limit", WorkflowProseRetryBiased, "GitHub API rate limit", RateLimited},
		{"workflow generic rate prose stays unknown", WorkflowProseRetryBiased, "provider rate limit", Unknown},
		{"blocked merge stays permanent to poller", GitHubPollerRetryBiased, "waiting for status timed out", Permanent},
		{"agent clone outranks rate limit", AgentRecoveryBiased, "git fetch: 429 rate limit", Transient},
		{"agent overload recovers", AgentRecoveryBiased, "provider overloaded", RateLimited},
		{"stream fallback keeps permissive 529", AgentStreamRecoveryBiased, "token 1529", RateLimited},
		{"llmexec rejects embedded 529", LLMExecRecoveryBiased, "token 1529", Unknown},
		{"llmexec accepts standalone 529", LLMExecRecoveryBiased, "HTTP 529", RateLimited},
		{"pr fix remote resumes", PRFixProseRetryBiased, "remote unreachable", Transient},
		{"pr fix credentials stop", PRFixProseRetryBiased, "missing credential", Permanent},
		{"unrecognized stays unknown", GitHubPollerRetryBiased, "something failed", Unknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Classify(tt.text, tt.policy); got != tt.want {
				t.Fatalf("Classify(%q, %q) = %q, want %q", tt.text, tt.policy, got, tt.want)
			}
		})
	}
}

func TestPolicyBiasIsExplicit(t *testing.T) {
	t.Parallel()
	tests := map[Policy]Bias{
		GitHubPollerRetryBiased:       RetryBiased,
		GHCommandEscalationBiased:     EscalationBiased,
		MonitorCooldownBiased:         CooldownBiased,
		GitHubTokenMintCooldownBiased: CooldownBiased,
		GitTransportEscalationBiased:  EscalationBiased,
		WorkflowProseRetryBiased:      RetryBiased,
		AgentRecoveryBiased:           RecoveryBiased,
		AgentStreamRecoveryBiased:     RecoveryBiased,
		LLMExecRecoveryBiased:         RecoveryBiased,
		PRFixProseRetryBiased:         RetryBiased,
	}
	for policy, want := range tests {
		if got := policy.Bias(); got != want {
			t.Errorf("%q.Bias() = %q, want %q", policy, got, want)
		}
	}
	if got := Policy("future-policy").Bias(); got != "" {
		t.Errorf("unknown policy bias = %q, want empty", got)
	}
}

func TestClassifyErr(t *testing.T) {
	t.Parallel()
	if got := ClassifyErr(nil, GitHubPollerRetryBiased); got != Unknown {
		t.Fatalf("nil = %q, want unknown", got)
	}
	if got := ClassifyErr(errors.New("HTTP 503"), GitHubPollerRetryBiased); got != Transient {
		t.Fatalf("HTTP 503 = %q, want transient", got)
	}
}
