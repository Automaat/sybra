package github

import "testing"

func TestFetchMyReviewState(t *testing.T) {
	// viewerLogin caches in a package global; seed it so the fake execer only
	// needs to answer the reviews call, and restore it afterward.
	viewerMu.Lock()
	prevViewer := cachedViewer
	cachedViewer = "me"
	viewerMu.Unlock()
	t.Cleanup(func() {
		viewerMu.Lock()
		cachedViewer = prevViewer
		viewerMu.Unlock()
	})

	tests := []struct {
		name string
		body string
		want MyReviewState
	}{
		{
			name: "pending draft only",
			body: `[{"state":"PENDING","user":{"login":"me"},"commit_id":"sha1"}]`,
			want: MyReviewState{Pending: true},
		},
		{
			name: "approved by me",
			body: `[{"state":"APPROVED","user":{"login":"me"},"commit_id":"sha1","submitted_at":"2026-01-01T00:00:00Z"}]`,
			want: MyReviewState{Submitted: true, Approved: true, ReviewedSHA: "sha1"},
		},
		{
			name: "my changes-requested wins; a peer's approval is ignored",
			body: `[{"state":"CHANGES_REQUESTED","user":{"login":"me"},"commit_id":"sha2","submitted_at":"2026-01-02T00:00:00Z"},{"state":"APPROVED","user":{"login":"peer"},"commit_id":"shaX","submitted_at":"2026-01-03T00:00:00Z"}]`,
			want: MyReviewState{Submitted: true, Approved: false, ReviewedSHA: "sha2"},
		},
		{
			name: "latest of my reviews wins",
			body: `[{"state":"CHANGES_REQUESTED","user":{"login":"me"},"commit_id":"old","submitted_at":"2026-01-01T00:00:00Z"},{"state":"APPROVED","user":{"login":"me"},"commit_id":"new","submitted_at":"2026-01-05T00:00:00Z"}]`,
			want: MyReviewState{Submitted: true, Approved: true, ReviewedSHA: "new"},
		},
		{
			// A COMMENTED review after an approval doesn't revoke it; the
			// approval verdict stands, while ReviewedSHA tracks the newer commit.
			name: "comment after approval keeps the approval",
			body: `[{"state":"APPROVED","user":{"login":"me"},"commit_id":"sha_a","submitted_at":"2026-01-01T00:00:00Z"},{"state":"COMMENTED","user":{"login":"me"},"commit_id":"sha_b","submitted_at":"2026-01-02T00:00:00Z"}]`,
			want: MyReviewState{Submitted: true, Approved: true, ReviewedSHA: "sha_b"},
		},
		{
			name: "pending plus a submitted comment review",
			body: `[{"state":"PENDING","user":{"login":"me"}},{"state":"COMMENTED","user":{"login":"me"},"commit_id":"sha3","submitted_at":"2026-01-02T00:00:00Z"}]`,
			want: MyReviewState{Pending: true, Submitted: true, ReviewedSHA: "sha3"},
		},
		{
			name: "no reviews",
			body: `[]`,
			want: MyReviewState{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fe := &fakeExecer{output: []byte(tt.body)}
			got, err := fetchMyReviewStateWith(fe, "o/r", 7)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}
