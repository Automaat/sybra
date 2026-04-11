package github

import (
	"fmt"
	"testing"
)

func TestFetchReviewsWith_success(t *testing.T) {
	t.Parallel()
	response := `{
		"data": {
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

	resetViewerCache()
	fe := &fakeExecer{output: []byte(response)}
	summary, err := fetchReviewsWith(fe)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fe.calls < 2 {
		t.Errorf("expected at least 2 calls (created + requested), got %d", fe.calls)
	}
	if len(summary.CreatedByMe) != 1 {
		t.Errorf("CreatedByMe len = %d, want 1", len(summary.CreatedByMe))
	}
	if len(summary.ReviewRequested) != 1 {
		t.Errorf("ReviewRequested len = %d, want 1", len(summary.ReviewRequested))
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
