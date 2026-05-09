package github

import (
	"encoding/json"
	"testing"
)

func TestIsInformationalCheck(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want bool
	}{
		{"codecov/patch", true},
		{"codecov/project", true},
		{"Codecov/Patch", true}, // case-insensitive prefix match
		{"sonarcloud/quality-gate", true},
		{"sonarsource/quality-gate", true},
		{"deepsource/test-coverage", true},
		{"format-lint", false},
		{"build", false},
		{"test", false},
		{"", false},
		{"codecovious/foo", false}, // prefix only matches with trailing slash
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isInformationalCheck(tt.name); got != tt.want {
				t.Errorf("isInformationalCheck(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestEffectiveCheckState_CheckRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		status     string
		conclusion string
		want       string
	}{
		{"running", "IN_PROGRESS", "", "PENDING"},
		{"queued", "QUEUED", "", "PENDING"},
		{"completed success", "COMPLETED", "SUCCESS", "SUCCESS"},
		{"completed failure", "COMPLETED", "FAILURE", "FAILURE"},
		{"completed timed out", "COMPLETED", "TIMED_OUT", "FAILURE"},
		{"completed startup failure", "COMPLETED", "STARTUP_FAILURE", "FAILURE"},
		{"completed action required", "COMPLETED", "ACTION_REQUIRED", "FAILURE"},
		{"completed neutral", "COMPLETED", "NEUTRAL", "SUCCESS"},
		{"completed cancelled", "COMPLETED", "CANCELLED", "SUCCESS"},
		{"completed skipped", "COMPLETED", "SKIPPED", "SUCCESS"},
		{"completed stale", "COMPLETED", "STALE", "SUCCESS"},
		{"completed unknown", "COMPLETED", "MARS_LANDING", "SUCCESS"},
		{"empty status, success conclusion", "", "SUCCESS", "SUCCESS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := gqlCheckContext{
				Typename:   "CheckRun",
				Status:     tt.status,
				Conclusion: tt.conclusion,
			}
			if got := effectiveCheckState(ctx); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEffectiveCheckState_StatusContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		state string
		want  string
	}{
		{"SUCCESS", "SUCCESS"},
		{"FAILURE", "FAILURE"},
		{"ERROR", "FAILURE"},
		{"PENDING", "PENDING"},
		{"EXPECTED", "PENDING"},
		{"", ""},
		{"NONSENSE", ""},
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			t.Parallel()
			ctx := gqlCheckContext{Typename: "StatusContext", State: tt.state}
			if got := effectiveCheckState(ctx); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEffectiveCheckState_NoTypename(t *testing.T) {
	t.Parallel()
	// Older gh versions don't return __typename — fall back to whichever
	// fields are populated.
	t.Run("infers CheckRun from status", func(t *testing.T) {
		t.Parallel()
		ctx := gqlCheckContext{Status: "COMPLETED", Conclusion: "FAILURE"}
		if got := effectiveCheckState(ctx); got != "FAILURE" {
			t.Errorf("got %q, want FAILURE", got)
		}
	})
	t.Run("infers StatusContext from state", func(t *testing.T) {
		t.Parallel()
		ctx := gqlCheckContext{State: "PENDING"}
		if got := effectiveCheckState(ctx); got != "PENDING" {
			t.Errorf("got %q, want PENDING", got)
		}
	})
	t.Run("empty context returns empty", func(t *testing.T) {
		t.Parallel()
		if got := effectiveCheckState(gqlCheckContext{}); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestRollupFromContexts(t *testing.T) {
	t.Parallel()
	checkRun := func(name, status, conclusion string) gqlCheckContext {
		return gqlCheckContext{Typename: "CheckRun", Name: name, Status: status, Conclusion: conclusion}
	}
	statusCtx := func(name, state string) gqlCheckContext {
		return gqlCheckContext{Typename: "StatusContext", Name: name, State: state}
	}

	tests := []struct {
		name        string
		contexts    []gqlCheckContext
		wantStatus  string
		wantPending bool
	}{
		{
			name:       "empty falls back to caller",
			contexts:   nil,
			wantStatus: "",
		},
		{
			name: "all real checks pass, codecov fails -> SUCCESS",
			contexts: []gqlCheckContext{
				checkRun("build", "COMPLETED", "SUCCESS"),
				checkRun("test", "COMPLETED", "SUCCESS"),
				checkRun("format-lint", "COMPLETED", "SUCCESS"),
				checkRun("codecov/patch", "COMPLETED", "FAILURE"),
				checkRun("codecov/project", "COMPLETED", "SUCCESS"),
			},
			wantStatus: "SUCCESS",
		},
		{
			name: "real check fails alongside codecov -> FAILURE",
			contexts: []gqlCheckContext{
				checkRun("build", "COMPLETED", "FAILURE"),
				checkRun("codecov/patch", "COMPLETED", "FAILURE"),
			},
			wantStatus: "FAILURE",
		},
		{
			name: "real check pending while codecov pending -> PENDING from real",
			contexts: []gqlCheckContext{
				checkRun("build", "IN_PROGRESS", ""),
				checkRun("codecov/patch", "IN_PROGRESS", ""),
			},
			wantStatus:  "PENDING",
			wantPending: true,
		},
		{
			name: "only codecov fails -> SUCCESS (no informational gating)",
			contexts: []gqlCheckContext{
				checkRun("codecov/patch", "COMPLETED", "FAILURE"),
			},
			// All filtered out -> "" so caller falls back to rollup.State
			wantStatus: "",
		},
		{
			name: "real failure with pending real -> FAILURE with hasPending",
			contexts: []gqlCheckContext{
				checkRun("build", "COMPLETED", "FAILURE"),
				checkRun("test", "IN_PROGRESS", ""),
			},
			wantStatus:  "FAILURE",
			wantPending: true,
		},
		{
			name: "mix of CheckRun and StatusContext",
			contexts: []gqlCheckContext{
				checkRun("build", "COMPLETED", "SUCCESS"),
				statusCtx("legacy-status", "FAILURE"),
			},
			wantStatus: "FAILURE",
		},
		{
			name: "only StatusContext informational",
			contexts: []gqlCheckContext{
				statusCtx("codecov/patch", "FAILURE"),
				statusCtx("codecov/project", "FAILURE"),
			},
			wantStatus: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, pending := rollupFromContexts(tt.contexts)
			if got != tt.wantStatus {
				t.Errorf("status = %q, want %q", got, tt.wantStatus)
			}
			if pending != tt.wantPending {
				t.Errorf("pending = %v, want %v", pending, tt.wantPending)
			}
		})
	}
}

func TestConvertPRs_filtersInformationalChecks(t *testing.T) {
	t.Parallel()
	// Mirrors the codecov-only failure on PR #186 that triggered the
	// pr-fix retry loop. With the filter, ciStatus must roll up to
	// SUCCESS so MatchTaskPRs does not raise a ci_failure issue.
	raw := `{
		"data": {
			"search": {
				"nodes": [
					{
						"number": 186,
						"title": "refactor: replace console",
						"url": "https://example.com/186",
						"author": {"login": "dev", "type": "User"},
						"repository": {"name": "creatorops", "nameWithOwner": "Automaat/creatorops"},
						"labels": {"nodes": []},
						"commits": {"nodes": [{"commit": {
							"oid": "abc",
							"statusCheckRollup": {
								"state": "FAILURE",
								"contexts": {"nodes": [
									{"__typename": "CheckRun", "name": "format-lint", "status": "COMPLETED", "conclusion": "SUCCESS"},
									{"__typename": "CheckRun", "name": "test", "status": "COMPLETED", "conclusion": "SUCCESS"},
									{"__typename": "CheckRun", "name": "build", "status": "COMPLETED", "conclusion": "SUCCESS"},
									{"__typename": "CheckRun", "name": "codecov/patch", "status": "COMPLETED", "conclusion": "FAILURE"},
									{"__typename": "CheckRun", "name": "codecov/project", "status": "COMPLETED", "conclusion": "SUCCESS"}
								]}
							}
						}}]},
						"reviewThreads": {"nodes": []}
					}
				]
			}
		}
	}`

	var resp gqlResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	prs := convertPRs(resp.Data.Search.Nodes, "")
	if len(prs) != 1 {
		t.Fatalf("got %d PRs, want 1", len(prs))
	}
	if prs[0].CIStatus != "SUCCESS" {
		t.Errorf("CIStatus = %q, want SUCCESS (codecov should be filtered)", prs[0].CIStatus)
	}
	if prs[0].HasPendingChecks {
		t.Errorf("HasPendingChecks = true, want false")
	}
}

func TestConvertPRs_realFailureBeatsCodecov(t *testing.T) {
	t.Parallel()
	// When a real CI check legitimately fails, the codecov filter must
	// not mask it.
	raw := `{
		"data": {"search": {"nodes": [{
			"number": 1,
			"title": "x",
			"url": "https://example.com/1",
			"author": {"login": "dev", "type": "User"},
			"repository": {"name": "r", "nameWithOwner": "o/r"},
			"labels": {"nodes": []},
			"commits": {"nodes": [{"commit": {
				"oid": "abc",
				"statusCheckRollup": {
					"state": "FAILURE",
					"contexts": {"nodes": [
						{"__typename": "CheckRun", "name": "build", "status": "COMPLETED", "conclusion": "FAILURE"},
						{"__typename": "CheckRun", "name": "codecov/patch", "status": "COMPLETED", "conclusion": "FAILURE"}
					]}
				}
			}}]},
			"reviewThreads": {"nodes": []}
		}]}}
	}`

	var resp gqlResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	prs := convertPRs(resp.Data.Search.Nodes, "")
	if len(prs) != 1 || prs[0].CIStatus != "FAILURE" {
		t.Fatalf("CIStatus = %q, want FAILURE", prs[0].CIStatus)
	}
}

func TestConvertPRs_emptyContextsFallsBackToRollupState(t *testing.T) {
	t.Parallel()
	// If GraphQL returns the rollup but no individual contexts (e.g.
	// older gh, suppressed check API), keep the rollup.State so the
	// new filter never silently zeros out CI status for legitimate PRs.
	raw := `{
		"data": {"search": {"nodes": [{
			"number": 1,
			"title": "x",
			"url": "https://example.com/1",
			"author": {"login": "dev", "type": "User"},
			"repository": {"name": "r", "nameWithOwner": "o/r"},
			"labels": {"nodes": []},
			"commits": {"nodes": [{"commit": {
				"oid": "abc",
				"statusCheckRollup": {"state": "FAILURE", "contexts": {"nodes": []}}
			}}]},
			"reviewThreads": {"nodes": []}
		}]}}
	}`

	var resp gqlResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	prs := convertPRs(resp.Data.Search.Nodes, "")
	if len(prs) != 1 || prs[0].CIStatus != "FAILURE" {
		t.Fatalf("CIStatus = %q, want FAILURE (fallback)", prs[0].CIStatus)
	}
}
