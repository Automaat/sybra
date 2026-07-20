package github

import (
	"fmt"
	"testing"
)

type recordingSequenceExecer struct {
	results []scriptedResult
	calls   [][]string
	callIdx int
}

func (r *recordingSequenceExecer) run(args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	i := r.callIdx
	r.callIdx++
	if i >= len(r.results) {
		return nil, fmt.Errorf("unexpected call %d", i+1)
	}
	return r.results[i].output, r.results[i].err
}

func TestRerunFailedChecksWith_UsesFailedRunID(t *testing.T) {
	t.Parallel()

	execer := &recordingSequenceExecer{
		results: []scriptedResult{
			{output: []byte(`{"headRefOid":"abc123"}`)},
			{output: []byte(`[{"databaseId":12345,"conclusion":"failure","headSha":"abc123"}]`)},
			{output: nil},
		},
	}

	if err := rerunFailedChecksWith(execer, "owner/repo", 42); err != nil {
		t.Fatalf("rerunFailedChecksWith() err = %v", err)
	}
	if len(execer.calls) != 3 {
		t.Fatalf("calls = %d, want 3", len(execer.calls))
	}
	want := []string{"run", "rerun", "12345", "--failed", "--repo", "owner/repo"}
	got := execer.calls[2]
	if len(got) != len(want) {
		t.Fatalf("rerun args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rerun arg[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	for _, arg := range execer.calls[1] {
		switch arg {
		case "--branch", "--status":
			t.Fatalf("run list args = %v, want commit-scoped local conclusion filtering", execer.calls[1])
		}
	}
	if !callHasArgPair(execer.calls[1], "--commit", "abc123") {
		t.Fatalf("run list args = %v, want --commit abc123", execer.calls[1])
	}
}

func TestLatestFailedRunIDOnCommitWith_BlockingConclusions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		conclusion string
	}{
		{"failure", "failure"},
		{"timed out", "timed_out"},
		{"startup failure", "startup_failure"},
		{"action required", "action_required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			execer := &fakeExecer{output: []byte(fmt.Sprintf(`[{"databaseId":987,"conclusion":%q,"headSha":"abc123"}]`, tt.conclusion))}
			got, err := latestFailedRunIDOnCommitWith(execer, "owner/repo", "abc123")
			if err != nil {
				t.Fatalf("latestFailedRunIDOnCommitWith() err = %v", err)
			}
			if got != 987 {
				t.Fatalf("run id = %d, want 987", got)
			}
		})
	}
}

func TestLatestFailedRunIDOnCommitWith_UsesNewestBlockingRunForHeadSHA(t *testing.T) {
	t.Parallel()

	execer := &fakeExecer{output: []byte(`[
		{"databaseId":1,"conclusion":"failure","headSha":"old456","headBranch":"feat/red"},
		{"databaseId":2,"conclusion":"success","headSha":"abc123","headBranch":"feat/red"},
		{"databaseId":3,"conclusion":"timed_out","headSha":"abc123","headBranch":"feat/red"},
		{"databaseId":4,"conclusion":"failure","headSha":"abc123","headBranch":"feat/red"}
	]`)}
	got, err := latestFailedRunIDOnCommitWith(execer, "owner/repo", "abc123")
	if err != nil {
		t.Fatalf("latestFailedRunIDOnCommitWith() err = %v", err)
	}
	if got != 3 {
		t.Fatalf("run id = %d, want 3", got)
	}
}

func TestLatestFailedRunIDOnCommitWith_ErrsWhenNoFailuresRemain(t *testing.T) {
	t.Parallel()

	execer := &fakeExecer{output: []byte(`[]`)}
	_, err := latestFailedRunIDOnCommitWith(execer, "owner/repo", "abc123")
	if err == nil {
		t.Fatal("latestFailedRunIDOnCommitWith() err = nil, want error")
	}
}

func callHasArgPair(call []string, flag, value string) bool {
	for i := 0; i < len(call)-1; i++ {
		if call[i] == flag && call[i+1] == value {
			return true
		}
	}
	return false
}
