package github

import (
	"fmt"
	"strings"
	"testing"
)

// pathExecer returns a canned body for the first stub whose key is a substring
// of the joined gh args, so a single fake can serve the pulls + check-runs legs
// of one fetchPRForMonitorViaREST call.
type pathExecer struct {
	responses map[string]string
	err       error
}

func (p *pathExecer) run(args ...string) ([]byte, error) {
	if p.err != nil {
		return nil, p.err
	}
	joined := strings.Join(args, " ")
	for key, body := range p.responses {
		if strings.Contains(joined, key) {
			return []byte(body), nil
		}
	}
	return nil, fmt.Errorf("no stub for: %s", joined)
}

func TestRestMergeable(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"dirty":    "CONFLICTING",
		"DIRTY":    "CONFLICTING",
		"unknown":  "UNKNOWN",
		"":         "UNKNOWN",
		"clean":    "MERGEABLE",
		"blocked":  "MERGEABLE",
		"unstable": "MERGEABLE",
		"behind":   "MERGEABLE",
	}
	for in, want := range cases {
		if got := restMergeable(in); got != want {
			t.Errorf("restMergeable(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFetchPRForMonitorViaREST_ConflictAndFailingCI(t *testing.T) {
	t.Parallel()
	e := &pathExecer{responses: map[string]string{
		"/pulls/42": `{"number":42,"title":"feat: x","html_url":"https://gh/pr/42",
			"state":"open","draft":false,"mergeable_state":"dirty",
			"head":{"ref":"feat/x","sha":"abc123"},"user":{"login":"me"},
			"labels":[{"name":"backend"}],"created_at":"t1","updated_at":"t2"}`,
		"/commits/abc123/check-runs": `{"check_runs":[
			{"status":"completed","conclusion":"success"},
			{"status":"completed","conclusion":"failure"}]}`,
	}}

	pr, open, err := fetchPRForMonitorViaREST(e, "Automaat/sybra", 42)
	if err != nil {
		t.Fatalf("fetchPRForMonitorViaREST: %v", err)
	}
	if !open {
		t.Fatal("expected open=true")
	}
	if pr.Mergeable != "CONFLICTING" {
		t.Errorf("Mergeable = %q, want CONFLICTING", pr.Mergeable)
	}
	if pr.CIStatus != "FAILURE" {
		t.Errorf("CIStatus = %q, want FAILURE", pr.CIStatus)
	}
	if pr.Number != 42 || pr.HeadRefName != "feat/x" || pr.HeadSHA != "abc123" {
		t.Errorf("unexpected basic fields: %+v", pr)
	}
	if pr.Repository != "Automaat/sybra" || pr.Author != "me" {
		t.Errorf("repo/author = %q/%q", pr.Repository, pr.Author)
	}
	// Thread-dependent fields must stay zero — REST cannot supply them, and
	// callers must not act on them.
	if pr.UnresolvedCount != 0 || pr.ActionableCount != 0 || pr.CopilotReviewed || pr.ReviewDecision != "" {
		t.Errorf("thread fields must be zero on REST path: %+v", pr)
	}
}

func TestFetchPRForMonitorViaREST_PendingCI(t *testing.T) {
	t.Parallel()
	e := &pathExecer{responses: map[string]string{
		"/pulls/7": `{"number":7,"state":"open","mergeable_state":"clean",
			"head":{"ref":"b","sha":"s7"}}`,
		"/commits/s7/check-runs": `{"check_runs":[
			{"status":"in_progress","conclusion":""}]}`,
	}}
	pr, open, err := fetchPRForMonitorViaREST(e, "o/r", 7)
	if err != nil || !open {
		t.Fatalf("open=%v err=%v", open, err)
	}
	if pr.Mergeable != "MERGEABLE" {
		t.Errorf("Mergeable = %q, want MERGEABLE", pr.Mergeable)
	}
	if pr.CIStatus != "PENDING" || !pr.HasPendingChecks {
		t.Errorf("CIStatus=%q pending=%v, want PENDING/true", pr.CIStatus, pr.HasPendingChecks)
	}
}

func TestFetchPRForMonitorViaREST_LegacyStatusFailure(t *testing.T) {
	t.Parallel()
	e := &pathExecer{responses: map[string]string{
		"/pulls/8": `{"number":8,"state":"open","mergeable_state":"clean",
			"head":{"ref":"b","sha":"s8"}}`,
		"/commits/s8/check-runs": `{"check_runs":[]}`,
		"/commits/s8/status": `{"statuses":[
			{"context":"ci/build","state":"failure"}]}`,
	}}
	pr, open, err := fetchPRForMonitorViaREST(e, "o/r", 8)
	if err != nil || !open {
		t.Fatalf("open=%v err=%v", open, err)
	}
	if pr.CIStatus != "FAILURE" || pr.HasPendingChecks {
		t.Errorf("CIStatus=%q pending=%v, want FAILURE/false", pr.CIStatus, pr.HasPendingChecks)
	}
}

func TestFetchPRForMonitorViaREST_FiltersInformationalAndCancelledChecks(t *testing.T) {
	t.Parallel()
	e := &pathExecer{responses: map[string]string{
		"/pulls/10": `{"number":10,"state":"open","mergeable_state":"clean",
			"head":{"ref":"b","sha":"s10"}}`,
		"/commits/s10/check-runs": `{"check_runs":[
			{"name":"codecov/patch","status":"completed","conclusion":"failure"},
			{"name":"ci/cancelled-optional","status":"completed","conclusion":"cancelled"},
			{"name":"ci/build","status":"completed","conclusion":"success"}]}`,
		"/commits/s10/status": `{"statuses":[]}`,
	}}
	pr, open, err := fetchPRForMonitorViaREST(e, "o/r", 10)
	if err != nil || !open {
		t.Fatalf("open=%v err=%v", open, err)
	}
	if pr.CIStatus != "SUCCESS" || pr.HasPendingChecks {
		t.Errorf("CIStatus=%q pending=%v, want SUCCESS/false", pr.CIStatus, pr.HasPendingChecks)
	}
}

func TestFetchPRForMonitorViaREST_ClosedPR(t *testing.T) {
	t.Parallel()
	e := &pathExecer{responses: map[string]string{
		"/pulls/9": `{"number":9,"state":"closed","head":{"sha":"x"}}`,
	}}
	_, open, err := fetchPRForMonitorViaREST(e, "o/r", 9)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if open {
		t.Error("closed PR should report open=false")
	}
}

func TestFetchPRStateViaREST_MergedAndClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "merged",
			body: `{"number":11,"state":"closed","merged_at":"2026-06-29T10:00:00Z","mergeable_state":"clean"}`,
			want: "MERGED",
		},
		{
			name: "closed unmerged",
			body: `{"number":12,"state":"closed","merged_at":null,"mergeable_state":"dirty"}`,
			want: "CLOSED",
		},
		{
			name: "open",
			body: `{"number":13,"state":"open","mergeable_state":"clean"}`,
			want: "OPEN",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &pathExecer{responses: map[string]string{"/pulls/": tt.body}}
			got, err := fetchPRStateViaREST(e, "o/r", 11)
			if err != nil {
				t.Fatalf("fetchPRStateViaREST: %v", err)
			}
			if got.State != tt.want {
				t.Errorf("State = %q, want %q", got.State, tt.want)
			}
		})
	}
}

func TestIsTransientError_BudgetExhausted(t *testing.T) {
	t.Parallel()
	if !IsTransientError(ErrBudgetExhausted) {
		t.Error("ErrBudgetExhausted must be classified transient")
	}
	if !IsTransientError(fmt.Errorf("wrapped: %w", ErrBudgetExhausted)) {
		t.Error("wrapped ErrBudgetExhausted must be classified transient")
	}
}
