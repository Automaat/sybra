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
			{output: []byte(`{"headRefName":"feat/rerun-me"}`)},
			{output: []byte(`[{"databaseId":12345}]`)},
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
}

func TestLatestFailedRunIDOnBranchWith_ErrsWhenNoFailuresRemain(t *testing.T) {
	t.Parallel()

	execer := &fakeExecer{output: []byte(`[]`)}
	_, err := latestFailedRunIDOnBranchWith(execer, "owner/repo", "feat/green")
	if err == nil {
		t.Fatal("latestFailedRunIDOnBranchWith() err = nil, want error")
	}
}
