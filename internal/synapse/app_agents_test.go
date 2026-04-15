package synapse

import (
	"testing"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/task"
)

// TestPickImplementationResumeSession guards against the regression where
// the resume-session walker grabbed the latest non-empty session_id from
// any prior run (including triage), which made the implementation agent
// launch with --resume against a session that lived in a different cwd.
// Claude CLI then bailed with subtype "error_during_execution", $0 cost,
// and verify_commits flipped the task to human-required immediately.
func TestPickImplementationResumeSession(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		runs []task.AgentRun
		want string
	}{
		{
			name: "empty",
			runs: nil,
			want: "",
		},
		{
			name: "only triage with session — must not resume",
			runs: []task.AgentRun{
				{Role: "triage", SessionID: "ses-triage"},
			},
			want: "",
		},
		{
			name: "triage then implementation — return implementation",
			runs: []task.AgentRun{
				{Role: "triage", SessionID: "ses-triage"},
				{Role: "", SessionID: "ses-impl"},
			},
			want: "ses-impl",
		},
		{
			name: "implementation then triage — skip triage, return impl",
			runs: []task.AgentRun{
				{Role: "", SessionID: "ses-impl-1"},
				{Role: "triage", SessionID: "ses-triage"},
			},
			want: "ses-impl-1",
		},
		{
			name: "explicit implementation role",
			runs: []task.AgentRun{
				{Role: string(agent.RoleImplementation), SessionID: "ses-impl-explicit"},
			},
			want: "ses-impl-explicit",
		},
		{
			name: "skip empty session_id, return previous impl",
			runs: []task.AgentRun{
				{Role: "", SessionID: "ses-old"},
				{Role: "", SessionID: ""},
			},
			want: "ses-old",
		},
		{
			name: "non-impl roles only — never resume",
			runs: []task.AgentRun{
				{Role: "plan", SessionID: "ses-plan"},
				{Role: "eval", SessionID: "ses-eval"},
				{Role: "triage", SessionID: "ses-triage"},
			},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := pickImplementationResumeSession(tc.runs)
			if got != tc.want {
				t.Errorf("pickImplementationResumeSession() = %q, want %q", got, tc.want)
			}
		})
	}
}
