package github

import (
	"fmt"
	"strings"
	"testing"
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
	for _, want := range []string{
		"reviewThreads(first: 100)",
		"id",
		"comments(last: 1)",
		"author { login }",
	} {
		if !strings.Contains(query, want) {
			t.Errorf("query missing %q:\n%s", want, query)
		}
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
