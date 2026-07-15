package github

import (
	"fmt"
	"strings"
	"testing"
)

// pathExecer returns a canned body for the stub whose key matches the request
// endpoint (the first non-flag "api" argument), so a single fake can serve the
// pulls + check-runs + reviews legs of one fetchPRForMonitorViaREST call.
// Matching is on the endpoint path exactly (trailing query stripped), not a
// substring of the joined args — a substring match would let "/pulls/42"
// wrongly match a request for "/pulls/42/reviews" (or vice versa,
// nondeterministically, since Go map iteration order is randomized).
type pathExecer struct {
	responses map[string]string
	err       error
}

func (p *pathExecer) run(args ...string) ([]byte, error) {
	if p.err != nil {
		return nil, p.err
	}
	endpoint := restAPIEndpoint(args)
	if body, ok := p.responses[endpoint]; ok {
		return []byte(body), nil
	}
	return nil, fmt.Errorf("no stub for endpoint %q (args: %s)", endpoint, strings.Join(args, " "))
}

// ghFlagsWithValue lists the `gh api` flags that consume the following argv
// element as their value, so restAPIEndpoint can skip both and land on the
// actual endpoint path instead of mistaking a flag value (e.g. "30s" from
// "--cache 30s") for it.
var ghFlagsWithValue = map[string]bool{
	"--cache": true, "--method": true, "-f": true, "-F": true,
	"-q": true, "-X": true, "--jq": true,
}

// restAPIEndpoint extracts the REST endpoint path from a `gh api ...` argv:
// the first non-flag argument after "api", skipping any flag/value pairs.
func restAPIEndpoint(args []string) string {
	for i, a := range args {
		if a != "api" {
			continue
		}
		for j := i + 1; j < len(args); {
			cur := args[j]
			if ghFlagsWithValue[cur] {
				j += 2
				continue
			}
			if strings.HasPrefix(cur, "-") {
				j++
				continue
			}
			if path, _, ok := strings.Cut(cur, "?"); ok {
				return path
			}
			return cur
		}
	}
	return strings.Join(args, " ")
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
		"repos/Automaat/sybra/pulls/42": `{"number":42,"title":"feat: x","html_url":"https://gh/pr/42",
			"state":"open","draft":false,"mergeable_state":"dirty",
			"head":{"ref":"feat/x","sha":"abc123"},"user":{"login":"me"},
			"labels":[{"name":"backend"}],"created_at":"t1","updated_at":"t2"}`,
		"repos/Automaat/sybra/commits/abc123/check-runs": `{"check_runs":[
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
		"repos/o/r/pulls/7": `{"number":7,"state":"open","mergeable_state":"clean",
			"head":{"ref":"b","sha":"s7"}}`,
		"repos/o/r/commits/s7/check-runs": `{"check_runs":[
			{"status":"in_progress","conclusion":""}]}`,
		"repos/o/r/commits/s7/status": `{"statuses":[]}`,
		"repos/o/r/pulls/7/reviews":   `[]`,
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
		"repos/o/r/pulls/8": `{"number":8,"state":"open","mergeable_state":"clean",
			"head":{"ref":"b","sha":"s8"}}`,
		"repos/o/r/commits/s8/check-runs": `{"check_runs":[]}`,
		"repos/o/r/commits/s8/status": `{"statuses":[
			{"context":"ci/build","state":"failure"}]}`,
		"repos/o/r/pulls/8/reviews": `[]`,
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
		"repos/o/r/pulls/10": `{"number":10,"state":"open","mergeable_state":"clean",
			"head":{"ref":"b","sha":"s10"}}`,
		"repos/o/r/commits/s10/check-runs": `{"check_runs":[
			{"name":"codecov/patch","status":"completed","conclusion":"failure"},
			{"name":"ci/cancelled-optional","status":"completed","conclusion":"cancelled"},
			{"name":"ci/build","status":"completed","conclusion":"success"}]}`,
		"repos/o/r/commits/s10/status": `{"statuses":[]}`,
		"repos/o/r/pulls/10/reviews":   `[]`,
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
		"repos/o/r/pulls/9": `{"number":9,"state":"closed","head":{"sha":"x"}}`,
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
			e := &pathExecer{responses: map[string]string{"repos/o/r/pulls/11": tt.body}}
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

// TestFetchPRForMonitorViaREST_CleanApproved verifies a clean, !draft PR whose
// CI is green and REST reviews show an APPROVED-at-head, non-dismissed review
// sets SourcedViaREST/RESTMergeableState/RESTCIFetched/RESTApproved — the
// signals the REST auto-merge gate needs.
func TestFetchPRForMonitorViaREST_CleanApproved(t *testing.T) {
	t.Parallel()
	e := &pathExecer{responses: map[string]string{
		"repos/o/r/pulls/20": `{"number":20,"state":"open","draft":false,"mergeable_state":"clean",
			"head":{"ref":"b","sha":"headsha"}}`,
		"repos/o/r/commits/headsha/check-runs": `{"check_runs":[
			{"status":"completed","conclusion":"success"}]}`,
		"repos/o/r/commits/headsha/status": `{"statuses":[]}`,
		"repos/o/r/pulls/20/reviews": `[
			{"state":"APPROVED","commit_id":"headsha","user":{"login":"alice"}}]`,
	}}
	pr, open, err := fetchPRForMonitorViaREST(e, "o/r", 20)
	if err != nil || !open {
		t.Fatalf("open=%v err=%v", open, err)
	}
	if !pr.SourcedViaREST {
		t.Error("SourcedViaREST must be true")
	}
	if pr.RESTMergeableState != "clean" {
		t.Errorf("RESTMergeableState = %q, want clean", pr.RESTMergeableState)
	}
	if !pr.RESTCIFetched {
		t.Error("RESTCIFetched must be true when both CI legs fetch cleanly")
	}
	if !pr.RESTApproved {
		t.Error("RESTApproved must be true for an APPROVED review at the current head")
	}
}

// TestFetchPRForMonitorViaREST_CIFetchFails verifies a failed check-runs leg
// leaves RESTCIFetched false even though the PR is otherwise clean — an empty
// CIStatus caused by a failed fetch must never read as green.
func TestFetchPRForMonitorViaREST_CIFetchFails(t *testing.T) {
	t.Parallel()
	e := &pathExecer{responses: map[string]string{
		"repos/o/r/pulls/21": `{"number":21,"state":"open","draft":false,"mergeable_state":"clean",
			"head":{"ref":"b","sha":"headsha21"}}`,
		"repos/o/r/commits/headsha21/status": `{"statuses":[]}`,
		"repos/o/r/pulls/21/reviews": `[
			{"state":"APPROVED","commit_id":"headsha21","user":{"login":"alice"}}]`,
	}}
	pr, open, err := fetchPRForMonitorViaREST(e, "o/r", 21)
	if err != nil || !open {
		t.Fatalf("open=%v err=%v", open, err)
	}
	if pr.RESTCIFetched {
		t.Error("RESTCIFetched must be false when the check-runs leg errors")
	}
}

// TestFetchPRForMonitorViaREST_StaleApproval verifies an APPROVED review whose
// commit_id doesn't match the PR's current head does not set RESTApproved.
func TestFetchPRForMonitorViaREST_StaleApproval(t *testing.T) {
	t.Parallel()
	e := &pathExecer{responses: map[string]string{
		"repos/o/r/pulls/22": `{"number":22,"state":"open","draft":false,"mergeable_state":"clean",
			"head":{"ref":"b","sha":"newsha"}}`,
		"repos/o/r/commits/newsha/check-runs": `{"check_runs":[]}`,
		"repos/o/r/commits/newsha/status":     `{"statuses":[]}`,
		"repos/o/r/pulls/22/reviews": `[
			{"state":"APPROVED","commit_id":"oldsha","user":{"login":"alice"}}]`,
	}}
	pr, open, err := fetchPRForMonitorViaREST(e, "o/r", 22)
	if err != nil || !open {
		t.Fatalf("open=%v err=%v", open, err)
	}
	if pr.RESTApproved {
		t.Error("a stale approval (commit_id != HeadSHA) must not set RESTApproved")
	}
}

// TestFetchPRForMonitorViaREST_CopilotCommentedOnly verifies a Copilot
// COMMENTED-only review never sets RESTApproved.
func TestFetchPRForMonitorViaREST_CopilotCommentedOnly(t *testing.T) {
	t.Parallel()
	e := &pathExecer{responses: map[string]string{
		"repos/o/r/pulls/23": `{"number":23,"state":"open","draft":false,"mergeable_state":"clean",
			"head":{"ref":"b","sha":"sha23"}}`,
		"repos/o/r/commits/sha23/check-runs": `{"check_runs":[]}`,
		"repos/o/r/commits/sha23/status":     `{"statuses":[]}`,
		"repos/o/r/pulls/23/reviews": `[
			{"state":"COMMENTED","commit_id":"sha23","user":{"login":"copilot-pull-request-reviewer[bot]"}}]`,
	}}
	pr, open, err := fetchPRForMonitorViaREST(e, "o/r", 23)
	if err != nil || !open {
		t.Fatalf("open=%v err=%v", open, err)
	}
	if pr.RESTApproved {
		t.Error("a Copilot COMMENTED-only review must not set RESTApproved")
	}
}

// TestFetchPRForMonitorViaREST_BlockedNotApproved verifies a non-clean
// mergeable_state (blocked/behind/unstable) skips the reviews fetch
// altogether, so RESTApproved never gets set from a non-clean PR.
func TestFetchPRForMonitorViaREST_BlockedNotApproved(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"blocked", "behind", "unstable", "unknown"} {
		e := &pathExecer{responses: map[string]string{
			"repos/o/r/pulls/24": fmt.Sprintf(`{"number":24,"state":"open","draft":false,"mergeable_state":%q,
				"head":{"ref":"b","sha":"sha24"}}`, state),
			"repos/o/r/commits/sha24/check-runs": `{"check_runs":[]}`,
			"repos/o/r/commits/sha24/status":     `{"statuses":[]}`,
		}}
		pr, open, err := fetchPRForMonitorViaREST(e, "o/r", 24)
		if err != nil || !open {
			t.Fatalf("state=%s: open=%v err=%v", state, open, err)
		}
		if pr.RESTApproved {
			t.Errorf("state=%s: RESTApproved must stay false for a non-clean mergeable_state", state)
		}
		if pr.RESTMergeableState != state {
			t.Errorf("RESTMergeableState = %q, want %q", pr.RESTMergeableState, state)
		}
	}
}

func TestRestApproval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		reviews []restReview
		headSHA string
		want    bool
	}{
		{"no reviews", nil, "h", false},
		{"approved at head", []restReview{{State: "APPROVED", CommitID: "h"}}, "h", true},
		{"approved stale sha", []restReview{{State: "APPROVED", CommitID: "old"}}, "h", false},
		{"changes requested blocks", []restReview{
			{State: "APPROVED", CommitID: "h"},
			{State: "CHANGES_REQUESTED", CommitID: "h"},
		}, "h", false},
		{"commented only is not approval", []restReview{{State: "COMMENTED", CommitID: "h"}}, "h", false},
		{"dismissed changes-request does not block", []restReview{
			{State: "APPROVED", CommitID: "h"},
			{State: "DISMISSED", CommitID: "h"},
		}, "h", true},
		{"stale changes-request superseded by later approval from same reviewer", []restReview{
			{State: "CHANGES_REQUESTED", CommitID: "old", SubmittedAt: "2026-01-01T00:00:00Z", User: struct {
				Login string `json:"login"`
			}{Login: "alice"}},
			{State: "APPROVED", CommitID: "h", SubmittedAt: "2026-01-02T00:00:00Z", User: struct {
				Login string `json:"login"`
			}{Login: "alice"}},
		}, "h", true},
		{"current changes-request from one reviewer blocks despite another's approval", []restReview{
			{State: "APPROVED", CommitID: "h", SubmittedAt: "2026-01-01T00:00:00Z", User: struct {
				Login string `json:"login"`
			}{Login: "alice"}},
			{State: "CHANGES_REQUESTED", CommitID: "h", SubmittedAt: "2026-01-02T00:00:00Z", User: struct {
				Login string `json:"login"`
			}{Login: "bob"}},
		}, "h", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := restApproval(tt.reviews, tt.headSHA); got != tt.want {
				t.Errorf("restApproval() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFetchCIStatusViaREST_OK(t *testing.T) {
	t.Parallel()
	e := &pathExecer{responses: map[string]string{
		"repos/o/r/commits/sha/check-runs": `{"check_runs":[{"status":"completed","conclusion":"success"}]}`,
		"repos/o/r/commits/sha/status":     `{"statuses":[]}`,
	}}
	status, pending, ok := fetchCIStatusViaREST(e, "o", "r", "sha")
	if !ok {
		t.Error("ok must be true when both legs fetch cleanly")
	}
	if status != "SUCCESS" || pending {
		t.Errorf("status=%q pending=%v, want SUCCESS/false", status, pending)
	}
}

func TestFetchCIStatusViaREST_CheckRunsLegErrors(t *testing.T) {
	t.Parallel()
	e := &pathExecer{responses: map[string]string{
		"repos/o/r/commits/sha/status": `{"statuses":[]}`,
	}}
	_, _, ok := fetchCIStatusViaREST(e, "o", "r", "sha")
	if ok {
		t.Error("ok must be false when the check-runs leg errors")
	}
}

func TestFetchCIStatusViaREST_EmptySHA(t *testing.T) {
	t.Parallel()
	e := &pathExecer{}
	_, _, ok := fetchCIStatusViaREST(e, "o", "r", "")
	if ok {
		t.Error("ok must be false for an empty sha (nothing was actually fetched)")
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
