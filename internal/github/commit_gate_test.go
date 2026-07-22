package github

import "testing"

func TestFetchCommitGateWith_AllRequiredChecksGreen(t *testing.T) {
	t.Parallel()

	e := &pathExecer{responses: map[string]string{
		"repos/o/r/commits/abc/check-runs": `{"check_runs":[
			{"name":"test","status":"completed","conclusion":"success"},
			{"name":"lint","status":"completed","conclusion":"success"}]}`,
		"repos/o/r/commits/abc/status": `{"statuses":[
			{"context":"lint","state":"success"},
			{"context":"test","state":"success"}]}`,
	}}

	got, err := fetchCommitGateWith(t.Context(), e, "o/r", "abc", []string{"test", "lint"})
	if err != nil {
		t.Fatalf("fetchCommitGateWith() err = %v", err)
	}
	if !got.Approved() {
		t.Fatalf("Approved() = false, want true: %+v", got)
	}
	if len(got.Succeeded) != 2 || len(got.Missing) != 0 || len(got.Pending) != 0 || len(got.Failed) != 0 {
		t.Fatalf("unexpected gate = %+v", got)
	}
}

func TestFetchCommitGateWith_MissingPendingAndFailedChecks(t *testing.T) {
	t.Parallel()

	e := &pathExecer{responses: map[string]string{
		"repos/o/r/commits/abc/check-runs": `{"check_runs":[
			{"name":"test","status":"completed","conclusion":"failure"},
			{"name":"build","status":"in_progress","conclusion":""}]}`,
		"repos/o/r/commits/abc/status": `{"statuses":[
			{"context":"test","state":"failure"},
			{"context":"build","state":"pending"}]}`,
	}}

	got, err := fetchCommitGateWith(t.Context(), e, "o/r", "abc", []string{"test", "build", "lint"})
	if err != nil {
		t.Fatalf("fetchCommitGateWith() err = %v", err)
	}
	if got.Approved() {
		t.Fatalf("Approved() = true, want false: %+v", got)
	}
	if len(got.Failed) != 1 || got.Failed[0] != "test" {
		t.Fatalf("Failed = %v, want [test]", got.Failed)
	}
	if len(got.Pending) != 1 || got.Pending[0] != "build" {
		t.Fatalf("Pending = %v, want [build]", got.Pending)
	}
	if len(got.Missing) != 1 || got.Missing[0] != "lint" {
		t.Fatalf("Missing = %v, want [lint]", got.Missing)
	}
}

func TestFetchCommitGateWith_PrefersWorseLegacyStatus(t *testing.T) {
	t.Parallel()

	e := &pathExecer{responses: map[string]string{
		"repos/o/r/commits/abc/check-runs": `{"check_runs":[
			{"name":"test","status":"completed","conclusion":"success"}]}`,
		"repos/o/r/commits/abc/status": `{"statuses":[
			{"context":"test","state":"failure"},
			{"context":"test","state":"success"}]}`,
	}}

	got, err := fetchCommitGateWith(t.Context(), e, "o/r", "abc", []string{"test"})
	if err != nil {
		t.Fatalf("fetchCommitGateWith() err = %v", err)
	}
	if got.Checks["test"] != "FAILURE" {
		t.Fatalf("Checks[test] = %q, want FAILURE", got.Checks["test"])
	}
}

func TestFetchCommitGateWith_CancelledCheckFailsClosed(t *testing.T) {
	t.Parallel()

	e := &pathExecer{responses: map[string]string{
		"repos/o/r/commits/abc/check-runs": `{"check_runs":[
			{"name":"test","status":"completed","conclusion":"cancelled"}]}`,
		"repos/o/r/commits/abc/status": `{"statuses":[]}`,
	}}

	got, err := fetchCommitGateWith(t.Context(), e, "o/r", "abc", []string{"test"})
	if err != nil {
		t.Fatalf("fetchCommitGateWith() err = %v", err)
	}
	if len(got.Failed) != 1 || got.Failed[0] != "test" {
		t.Fatalf("Failed = %v, want [test]", got.Failed)
	}
	if got.Checks["test"] != "FAILURE" {
		t.Fatalf("Checks[test] = %q, want FAILURE", got.Checks["test"])
	}
}

func TestFetchCommitGateWith_NeutralAndSkippedChecksPass(t *testing.T) {
	t.Parallel()

	e := &pathExecer{responses: map[string]string{
		"repos/o/r/commits/abc/check-runs": `{"check_runs":[
			{"name":"test","status":"completed","conclusion":"neutral"},
			{"name":"lint","status":"completed","conclusion":"skipped"}]}`,
		"repos/o/r/commits/abc/status": `{"statuses":[]}`,
	}}

	got, err := fetchCommitGateWith(t.Context(), e, "o/r", "abc", []string{"test", "lint"})
	if err != nil {
		t.Fatalf("fetchCommitGateWith() err = %v", err)
	}
	if !got.Approved() {
		t.Fatalf("Approved() = false, want true: %+v", got)
	}
	if got.Checks["test"] != "SUCCESS" || got.Checks["lint"] != "SUCCESS" {
		t.Fatalf("Checks = %v, want test/lint SUCCESS", got.Checks)
	}
	if len(got.Succeeded) != 2 || len(got.Failed) != 0 || len(got.Pending) != 0 || len(got.Missing) != 0 {
		t.Fatalf("unexpected gate = %+v", got)
	}
}

func TestFetchCommitGateWith_StatusFetchFailureAllowedWhenCheckRunsResolveAll(t *testing.T) {
	t.Parallel()

	e := &pathExecer{responses: map[string]string{
		"repos/o/r/commits/abc/check-runs": `{"check_runs":[
			{"name":"test","status":"completed","conclusion":"success"},
			{"name":"lint","status":"completed","conclusion":"success"}]}`,
	}}

	got, err := fetchCommitGateWith(t.Context(), e, "o/r", "abc", []string{"test", "lint"})
	if err != nil {
		t.Fatalf("fetchCommitGateWith() err = %v", err)
	}
	if !got.Approved() {
		t.Fatalf("Approved() = false, want true: %+v", got)
	}
	if len(got.Succeeded) != 2 || len(got.Missing) != 0 || len(got.Pending) != 0 || len(got.Failed) != 0 {
		t.Fatalf("unexpected gate = %+v", got)
	}
}

func TestFetchCommitGateWith_StatusFetchFailureFailsWhenRequiredUnresolved(t *testing.T) {
	t.Parallel()

	e := &pathExecer{responses: map[string]string{
		"repos/o/r/commits/abc/check-runs": `{"check_runs":[
			{"name":"test","status":"completed","conclusion":"success"}]}`,
	}}

	if _, err := fetchCommitGateWith(t.Context(), e, "o/r", "abc", []string{"test", "lint"}); err == nil {
		t.Fatal("fetchCommitGateWith() err = nil, want error")
	}
}

func TestFetchCommitGateWith_CheckRunFetchFailureAllowedWhenStatusesResolveAll(t *testing.T) {
	t.Parallel()

	e := &pathExecer{responses: map[string]string{
		"repos/o/r/commits/abc/status": `{"statuses":[
			{"context":"test","state":"success"},
			{"context":"lint","state":"success"}]}`,
	}}

	got, err := fetchCommitGateWith(t.Context(), e, "o/r", "abc", []string{"test", "lint"})
	if err != nil {
		t.Fatalf("fetchCommitGateWith() err = %v", err)
	}
	if !got.Approved() {
		t.Fatalf("Approved() = false, want true: %+v", got)
	}
	if len(got.Succeeded) != 2 || len(got.Missing) != 0 || len(got.Pending) != 0 || len(got.Failed) != 0 {
		t.Fatalf("unexpected gate = %+v", got)
	}
}

func TestFetchCommitGateWith_FailsClosedOnCheckRunFetchFailure(t *testing.T) {
	t.Parallel()

	e := &pathExecer{responses: map[string]string{
		"repos/o/r/commits/abc/status": `{"statuses":[]}`,
	}}

	if _, err := fetchCommitGateWith(t.Context(), e, "o/r", "abc", []string{"test"}); err == nil {
		t.Fatal("fetchCommitGateWith() err = nil, want error")
	}
}

func TestFetchCommitGateWith_FailsClosedOnEmptyRequiredChecks(t *testing.T) {
	t.Parallel()

	e := &pathExecer{}
	if _, err := fetchCommitGateWith(t.Context(), e, "o/r", "abc", nil); err == nil {
		t.Fatal("fetchCommitGateWith() err = nil, want error")
	}
}
