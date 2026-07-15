package github

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestFetchReviewsWith_success(t *testing.T) {
	t.Parallel()
	createdResponse := `{
		"data": {
			"viewer": {"login": "me"},
			"search": {
				"nodes": [
					{
						"number": 1,
						"title": "my PR",
						"url": "https://github.com/o/r/pull/1",
						"author": {"login": "me", "type": "User"},
						"repository": {"name": "r", "nameWithOwner": "o/r"},
						"labels": {"nodes": []},
						"commits": {"nodes": []},
						"reviewThreads": {"nodes": []}
					}
				]
			}
		}
	}`
	requestedResponse := `{
		"data": {
			"viewer": {"login": "me"},
			"search": {
				"nodes": [
					{
						"number": 2,
						"title": "review me",
						"url": "https://github.com/o/r/pull/2",
						"author": {"login": "peer", "type": "User"},
						"repository": {"name": "r", "nameWithOwner": "o/r"},
						"labels": {"nodes": []},
						"commits": {"nodes": []},
						"reviewThreads": {"nodes": []},
						"latestReviews": {"nodes": []}
					}
				]
			}
		}
	}`
	reviewedResponse := `{
		"data": {
			"viewer": {"login": "me"},
			"search": {
				"nodes": [
					{
						"number": 3,
						"title": "approved by me",
						"url": "https://github.com/o/r/pull/3",
						"author": {"login": "peer", "type": "User"},
						"repository": {"name": "r", "nameWithOwner": "o/r"},
						"labels": {"nodes": []},
						"commits": {"nodes": []},
						"reviewThreads": {"nodes": []},
						"latestReviews": {"nodes": [{"state": "APPROVED", "author": {"login": "me"}}]}
					},
					{
						"number": 4,
						"title": "commented by me",
						"url": "https://github.com/o/r/pull/4",
						"author": {"login": "peer", "type": "User"},
						"repository": {"name": "r", "nameWithOwner": "o/r"},
						"labels": {"nodes": []},
						"commits": {"nodes": []},
						"reviewThreads": {"nodes": []},
						"latestReviews": {"nodes": [{"state": "COMMENTED", "author": {"login": "me"}}]}
					}
				]
			}
		}
	}`

	fe := &sequenceExecer{outputs: [][]byte{
		[]byte(createdResponse),
		[]byte(requestedResponse),
		[]byte(reviewedResponse),
	}}
	summary, err := fetchReviewsWith(fe)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fe.calls != 3 {
		t.Errorf("calls = %d, want 3", fe.calls)
	}
	if len(summary.CreatedByMe) != 1 {
		t.Errorf("CreatedByMe len = %d, want 1", len(summary.CreatedByMe))
	}
	if len(summary.ReviewRequested) != 1 {
		t.Errorf("ReviewRequested len = %d, want 1", len(summary.ReviewRequested))
	}
	if len(summary.ReviewedByMe) != 1 {
		t.Fatalf("ReviewedByMe len = %d, want 1", len(summary.ReviewedByMe))
	}
	if summary.ReviewedByMe[0].Number != 3 {
		t.Errorf("ReviewedByMe[0].Number = %d, want 3", summary.ReviewedByMe[0].Number)
	}
}

func TestFetchReviewsWith_failedCheckRunConclusion(t *testing.T) {
	t.Parallel()
	createdResponse := `{
		"data": {
			"viewer": {"login": "me"},
			"search": {
				"nodes": [
					{
						"number": 1,
						"title": "my PR",
						"url": "https://github.com/o/r/pull/1",
						"author": {"login": "me", "type": "User"},
						"repository": {"name": "r", "nameWithOwner": "o/r"},
						"labels": {"nodes": []},
						"commits": {"nodes": [{
							"commit": {
								"oid": "abc123",
								"statusCheckRollup": {
									"state": "FAILURE",
									"contexts": {"nodes": [
										{"__typename": "CheckRun", "name": "check", "status": "COMPLETED", "conclusion": "FAILURE"}
									]}
								}
							}
						}]},
						"reviewThreads": {"nodes": []},
						"latestReviews": {"nodes": []}
					}
				]
			}
		}
	}`
	emptyResponse := `{
		"data": {
			"viewer": {"login": "me"},
			"search": {"nodes": []}
		}
	}`

	fe := &sequenceExecer{outputs: [][]byte{
		[]byte(createdResponse),
		[]byte(emptyResponse),
		[]byte(emptyResponse),
	}}
	summary, err := fetchReviewsWith(fe)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := summary.CreatedByMe[0].CIStatus; got != "FAILURE" {
		t.Errorf("CIStatus = %q, want FAILURE", got)
	}
	if summary.CreatedByMe[0].HasPendingChecks {
		t.Error("HasPendingChecks = true, want false")
	}
}

func TestFetchReviewsWith_actionableReviewThreads(t *testing.T) {
	t.Parallel()
	createdResponse := `{
		"data": {
			"viewer": {"login": "me"},
			"search": {
				"nodes": [
					{
						"number": 1,
						"title": "my PR",
						"url": "https://github.com/o/r/pull/1",
						"author": {"login": "me", "type": "User"},
						"repository": {"name": "r", "nameWithOwner": "o/r"},
						"labels": {"nodes": []},
						"commits": {"nodes": []},
						"reviewThreads": {"nodes": [
							{
								"id": "T1",
								"isResolved": false,
								"comments": {"nodes": [{"author": {"login": "copilot-pull-request-reviewer"}}]}
							}
						]},
						"latestReviews": {"nodes": [
							{"state": "COMMENTED", "author": {"login": "copilot-pull-request-reviewer"}}
						]}
					}
				]
			}
		}
	}`
	emptyResponse := `{
		"data": {
			"viewer": {"login": "me"},
			"search": {"nodes": []}
		}
	}`

	fe := &sequenceExecer{outputs: [][]byte{
		[]byte(createdResponse),
		[]byte(emptyResponse),
		[]byte(emptyResponse),
	}}
	summary, err := fetchReviewsWith(fe)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pr := summary.CreatedByMe[0]
	if pr.UnresolvedCount != 1 {
		t.Errorf("UnresolvedCount = %d, want 1", pr.UnresolvedCount)
	}
	if pr.ActionableCount != 1 {
		t.Errorf("ActionableCount = %d, want 1", pr.ActionableCount)
	}
	if pr.FeedbackSig == "" {
		t.Error("FeedbackSig is empty, want unresolved thread id included")
	}
	if !pr.CopilotReviewed {
		t.Error("CopilotReviewed = false, want true")
	}
}

func TestFetchReviewSearchWith_queryRequestsActionableThreadSignals(t *testing.T) {
	t.Parallel()
	fe := &recordingExecer{output: []byte(`{
		"data": {
			"viewer": {"login": "me"},
			"search": {"nodes": []}
		}
	}`)}

	if _, err := fetchReviewSearchWith(fe, "is:pr is:open author:@me"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var query string
	for _, arg := range fe.lastArgs {
		if after, ok := strings.CutPrefix(arg, "query="); ok {
			query = after
			break
		}
	}
	if query == "" {
		t.Fatal("graphql query argument not captured")
	}
	normalized := strings.Join(strings.Fields(query), " ")
	want := "reviewThreads(first: 100) { nodes { id isResolved comments(last: 1) { nodes { author { login } } } } }"
	if !strings.Contains(normalized, want) {
		t.Errorf("reviewThreads selection missing actionable thread signals\nwant fragment: %s\nquery: %s", want, normalized)
	}
}

func TestFetchReviewsWith_graphqlError(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte(`{"errors":[{"message":"rate limited"}]}`)}
	_, err := fetchReviewsWith(fe)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestFetchReviewsWith_firstCallFails(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{
		output: []byte("auth error"),
		err:    fmt.Errorf("exit 1"),
	}
	_, err := fetchReviewsWith(fe)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestHasPendingReview_pending(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte(`[{"state":"COMMENTED"},{"state":"PENDING"}]`)}
	got, err := hasPendingReviewWith(fe, "owner/repo", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Error("expected pending review, got false")
	}
}

func TestHasPendingReview_noPending(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte(`[{"state":"APPROVED"},{"state":"COMMENTED"}]`)}
	got, err := hasPendingReviewWith(fe, "owner/repo", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Error("expected no pending review, got true")
	}
}

func TestHasPendingReview_empty(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte(`[]`)}
	got, err := hasPendingReviewWith(fe, "owner/repo", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Error("expected no pending review, got true")
	}
}

func TestHasPendingReview_error(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte("not found"), err: fmt.Errorf("exit 1")}
	_, err := hasPendingReviewWith(fe, "owner/repo", 42)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestFetchPRsForMonitorWith_batchesIntoOneCall(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte(`{
		"data": {
			"viewer": {"login": "me"},
			"repo0": {
				"pullRequest": {
					"number": 1,
					"title": "one",
					"url": "https://github.com/o/r/pull/1",
					"state": "OPEN",
					"author": {"login": "me", "type": "User"},
					"repository": {"name": "r", "nameWithOwner": "o/r"},
					"labels": {"nodes": []},
					"commits": {"nodes": []},
					"reviewThreads": {"nodes": []},
					"latestReviews": {"nodes": []}
				}
			},
			"repo1": {
				"pullRequest": {
					"number": 2,
					"title": "two",
					"url": "https://github.com/o/r/pull/2",
					"state": "MERGED",
					"author": {"login": "peer", "type": "User"},
					"repository": {"name": "r", "nameWithOwner": "o/r"},
					"labels": {"nodes": []},
					"commits": {"nodes": []},
					"reviewThreads": {"nodes": []},
					"latestReviews": {"nodes": []}
				}
			}
		}
	}`)}

	refs := []PRRef{{Repo: "o/r", Number: 1}, {Repo: "o/r", Number: 2}}
	results := fetchPRsForMonitorWith(fe, refs)

	if fe.calls != 1 {
		t.Fatalf("calls = %d, want 1", fe.calls)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0].Err != nil || !results[0].Open || results[0].PR.Title != "one" {
		t.Fatalf("results[0] = %+v", results[0])
	}
	if results[1].Err != nil || results[1].Open {
		t.Fatalf("results[1] = %+v, want closed PR with no error", results[1])
	}
}

func TestFetchPRsForMonitorWith_chunksOverLimit(t *testing.T) {
	t.Parallel()
	fe := &sequenceExecer{
		outputs: [][]byte{
			[]byte(`{"data": {"viewer": {"login": "me"}}}`),
			[]byte(`{"data": {"viewer": {"login": "me"}}}`),
		},
	}

	refs := make([]PRRef, maxBatchPRsPerQuery+3)
	for i := range refs {
		refs[i] = PRRef{Repo: "o/r", Number: i + 1}
	}
	results := fetchPRsForMonitorWith(fe, refs)

	if fe.calls != 2 {
		t.Fatalf("calls = %d, want 2", fe.calls)
	}
	if len(results) != len(refs) {
		t.Fatalf("len(results) = %d, want %d", len(results), len(refs))
	}
}

func TestFetchPRsForMonitorWith_graphqlError(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte(`{"errors":[{"message":"rate limited"}]}`)}
	results := fetchPRsForMonitorWith(fe, []PRRef{{Repo: "o/r", Number: 1}})
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("results = %+v, want single error result", results)
	}
}

func TestFetchPRsForMonitorWith_invalidRefSkipsQuery(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{}
	results := fetchPRsForMonitorWith(fe, []PRRef{{Repo: "bad", Number: 1}})
	if fe.calls != 0 {
		t.Fatalf("calls = %d, want 0", fe.calls)
	}
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("results = %+v, want single error result", results)
	}
}

// TestFetchPRBatchWith_partialError locks that a GraphQL error scoped to one
// alias (via errors[].path) only fails that alias — the other aliases in the
// same batch, which have valid data alongside the error, must still resolve.
func TestFetchPRBatchWith_partialError(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte(`{
		"data": {
			"viewer": {"login": "me"},
			"repo0": {
				"pullRequest": {
					"number": 1,
					"title": "one",
					"url": "https://github.com/o/r/pull/1",
					"state": "OPEN",
					"author": {"login": "me", "type": "User"},
					"repository": {"name": "r", "nameWithOwner": "o/r"},
					"labels": {"nodes": []},
					"commits": {"nodes": []},
					"reviewThreads": {"nodes": []},
					"latestReviews": {"nodes": []}
				}
			},
			"repo1": null
		},
		"errors": [
			{"message": "Could not resolve to a PullRequest", "path": ["repo1", "pullRequest"]}
		]
	}`)}

	refs := []PRRef{{Repo: "o/r", Number: 1}, {Repo: "o/r", Number: 2}}
	results := fetchPRsForMonitorWith(fe, refs)

	if fe.calls != 1 {
		t.Fatalf("calls = %d, want 1", fe.calls)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0].Err != nil || !results[0].Open || results[0].PR.Title != "one" {
		t.Fatalf("results[0] = %+v, want valid open PR unaffected by repo1's error", results[0])
	}
	if results[1].Err == nil {
		t.Fatalf("results[1] = %+v, want error for the aliased PR that GraphQL reported", results[1])
	}
}

// TestFetchPRBatchWith_globalErrorFailsWholeBatch locks that a query-level
// GraphQL error with no path (no per-alias distinction to salvage) still
// fails every ref in the batch, same as before this change.
func TestFetchPRBatchWith_globalErrorFailsWholeBatch(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte(`{"errors":[{"message":"rate limited"}]}`)}
	refs := []PRRef{{Repo: "o/r", Number: 1}, {Repo: "o/r", Number: 2}}
	results := fetchPRsForMonitorWith(fe, refs)
	if len(results) != 2 || results[0].Err == nil || results[1].Err == nil {
		t.Fatalf("results = %+v, want both refs to error", results)
	}
}

// TestFetchPRBatchWith_viewerErrorFailsWholeBatch locks that a GraphQL error
// scoped to the shared top-level "viewer" field (not a "repoN" ref alias) is
// treated as global, not silently dropped or misfiled under a "viewer" key
// that no ref ever looks up.
func TestFetchPRBatchWith_viewerErrorFailsWholeBatch(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte(`{
		"data": {
			"repo0": {
				"pullRequest": {
					"number": 1,
					"title": "one",
					"url": "https://github.com/o/r/pull/1",
					"state": "OPEN",
					"author": {"login": "me", "type": "User"},
					"repository": {"name": "r", "nameWithOwner": "o/r"},
					"labels": {"nodes": []},
					"commits": {"nodes": []},
					"reviewThreads": {"nodes": []},
					"latestReviews": {"nodes": []}
				}
			}
		},
		"errors": [{"message": "viewer unavailable", "path": ["viewer"]}]
	}`)}

	refs := []PRRef{{Repo: "o/r", Number: 1}}
	results := fetchPRsForMonitorWith(fe, refs)
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Err == nil {
		t.Fatalf("results[0] = %+v, want an error for the viewer-scoped failure, not a silently reported open PR", results[0])
	}
}

// TestFetchPRBatchWith_updatesGateForNextChunk locks the mechanism
// fetchPRsForMonitorWith's per-chunk gate recheck depends on: a chunk
// response carrying low-budget rate-limit headers must update ghGate so that
// shouldSkipOptional("graphql", priorityMergePath) reports true immediately after, without
// waiting for a separate /rate_limit refresh. (The recheck itself is gated on
// runtimeCacheEnabled(e), i.e. the real defaultExecer, so it cannot be driven
// end-to-end through a fake execer — see the identical constraint on the
// pre-loop check a few lines above in fetchPRsForMonitorWith.)
func TestFetchPRBatchWith_updatesGateForNextChunk(t *testing.T) {
	orig := ghGate
	t.Cleanup(func() { ghGate = orig })
	ghGate = newGHRequestGate()

	resetAt := time.Now().Add(time.Hour).Unix()
	chunk1 := fmt.Sprintf("HTTP/1.1 200 OK\n"+
		"x-ratelimit-resource: graphql\n"+
		"x-ratelimit-remaining: 0\n"+
		"x-ratelimit-limit: 5000\n"+
		"x-ratelimit-reset: %d\n\n"+
		`{"data": {"viewer": {"login": "me"}}}`, resetAt)

	fe := &fakeExecer{output: []byte(chunk1)}
	refs := []PRRef{{Repo: "o/r", Number: 1}}

	if ghGate.shouldSkipOptional("graphql", priorityMergePath) {
		t.Fatal("gate should not skip graphql before any call")
	}
	fetchPRBatchWith(fe, refs)
	if !ghGate.shouldSkipOptional("graphql", priorityMergePath) {
		t.Fatal("want gate to skip graphql after a chunk response reports zero remaining budget")
	}
}
