package workflow

import (
	"testing"
	"time"
)

// The same-fingerprint cap exists to stop a repair loop that makes no
// progress. It could not tell "the agent tried and failed" from "the agent
// never touched the file": a run that ended without committing spent the
// budget just the same, so a trivially fixable finding escalated on an attempt
// that never happened.
func TestRewindRetry_FingerprintNotChargedWithoutCommits(t *testing.T) {
	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())

	policy := func(ti TaskInfo) rewindRetryPolicy {
		return rewindRetryPolicy{
			counterKey:             "step.verify.auto_fix",
			max:                    5,
			rewindStep:             "implement",
			backoff:                func(int) time.Duration { return 0 },
			fingerprint:            "fp-unused-funcs",
			maxSameFingerprintRuns: 1,
			attemptProducedWork:    lastAuthorRunProducedWork,
			reason:                 func(int) string { return "retry" },
		}
	}

	t.Run("run that committed charges the occurrence", func(t *testing.T) {
		wfExec := &Execution{Variables: map[string]string{}}
		ti := TaskInfo{ID: "t1", Status: "in-progress", AgentRuns: []AgentRunInfo{
			{AgentID: "a1", Role: "implementation", HeadSHA: "deadbeef"},
		}}
		if armed, _, _ := engine.rewindRetry("t1", wfExec, ti, policy(ti)); !armed {
			t.Fatal("first arm should be allowed")
		}
		if armed, _, _ := engine.rewindRetry("t1", wfExec, ti, policy(ti)); armed {
			t.Error("a committed attempt must spend the same-fingerprint budget")
		}
	})

	t.Run("run that committed nothing does not", func(t *testing.T) {
		wfExec := &Execution{Variables: map[string]string{}}
		ti := TaskInfo{ID: "t1", Status: "in-progress", AgentRuns: []AgentRunInfo{
			{AgentID: "a1", Role: "implementation"},
		}}
		if armed, _, _ := engine.rewindRetry("t1", wfExec, ti, policy(ti)); !armed {
			t.Fatal("first arm should be allowed")
		}
		if armed, _, _ := engine.rewindRetry("t1", wfExec, ti, policy(ti)); !armed {
			t.Error("a run that never committed did not attempt the fix; it must not spend the budget")
		}
	})

	// The generic ceiling still terminates a run that never commits, so the
	// evidence gate cannot spin forever.
	t.Run("absolute ceiling still bounds the loop", func(t *testing.T) {
		wfExec := &Execution{Variables: map[string]string{}}
		ti := TaskInfo{ID: "t1", Status: "in-progress", AgentRuns: []AgentRunInfo{
			{AgentID: "a1", Role: "implementation"},
		}}
		p := policy(ti)
		p.max = 3
		armedCount := 0
		for range 10 {
			if armed, _, _ := engine.rewindRetry("t1", wfExec, ti, p); armed {
				armedCount++
			}
		}
		if armedCount != 3 {
			t.Errorf("armed %d times, want exactly the ceiling of 3", armedCount)
		}
	})
}

func TestLastAuthorRunProducedWork(t *testing.T) {
	tests := []struct {
		name string
		runs []AgentRunInfo
		want bool
	}{
		{"no runs at all charges the occurrence", nil, true},
		{"author run with a head sha", []AgentRunInfo{{Role: "implementation", HeadSHA: "abc"}}, true},
		{"author run with a commit source", []AgentRunInfo{{Role: "implementation", FinalCommitSource: "agent"}}, true},
		{"author run with neither", []AgentRunInfo{{Role: "implementation"}}, false},
		{
			// Verifier roles produce no commit by design and must not be read
			// as the failed attempt.
			name: "verifier run is skipped in favour of the author run",
			runs: []AgentRunInfo{{Role: "implementation", HeadSHA: "abc"}, {Role: "review"}},
			want: true,
		},
		{
			name: "most recent author run wins",
			runs: []AgentRunInfo{{Role: "implementation", HeadSHA: "abc"}, {Role: "implementation"}},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := lastAuthorRunProducedWork(TaskInfo{AgentRuns: tc.runs}); got != tc.want {
				t.Errorf("lastAuthorRunProducedWork = %v, want %v", got, tc.want)
			}
		})
	}
}
