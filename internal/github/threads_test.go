package github

import "testing"

func TestFetchReviewThreads_parse(t *testing.T) {
	t.Parallel()
	body := `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
		{"id":"T1","isResolved":false,"isOutdated":true,"comments":{"nodes":[{"author":{"login":"copilot-pull-request-reviewer[bot]"}}]}},
		{"id":"T2","isResolved":true,"isOutdated":false,"comments":{"nodes":[{"author":{"login":"dev"}}]}},
		{"id":"T3","isResolved":false,"isOutdated":false,"comments":{"nodes":[]}}
	]}}}}}`
	fe := &fakeExecer{output: []byte(body)}

	threads, err := fetchReviewThreadsWith(fe, "o/r", 9)
	if err != nil {
		t.Fatalf("fetchReviewThreadsWith: %v", err)
	}
	if len(threads) != 3 {
		t.Fatalf("got %d threads, want 3", len(threads))
	}
	if threads[0].ID != "T1" || threads[0].AuthorLogin != "copilot-pull-request-reviewer[bot]" || !threads[0].IsOutdated || threads[0].IsResolved {
		t.Errorf("thread[0] = %+v", threads[0])
	}
	if threads[1].AuthorLogin != "dev" || !threads[1].IsResolved {
		t.Errorf("thread[1] = %+v", threads[1])
	}
	if threads[2].AuthorLogin != "" { // no comments → empty author
		t.Errorf("thread[2] author = %q, want empty", threads[2].AuthorLogin)
	}
}

func TestFetchReviewThreads_invalidRepo(t *testing.T) {
	t.Parallel()
	if _, err := fetchReviewThreadsWith(&fakeExecer{}, "noslash", 1); err == nil {
		t.Fatal("want error for invalid repo, got nil")
	}
}

func TestResolveReviewThread(t *testing.T) {
	t.Parallel()
	t.Run("success", func(t *testing.T) {
		t.Parallel()
		fe := &fakeExecer{output: []byte(`{"data":{"resolveReviewThread":{"thread":{"id":"T1"}}}}`)}
		if err := resolveReviewThreadWith(fe, "T1"); err != nil {
			t.Fatalf("resolveReviewThreadWith: %v", err)
		}
	})
	t.Run("graphql error", func(t *testing.T) {
		t.Parallel()
		fe := &fakeExecer{output: []byte(`{"errors":[{"message":"thread not found"}]}`)}
		if err := resolveReviewThreadWith(fe, "T1"); err == nil {
			t.Fatal("want error from graphql errors, got nil")
		}
	})
}
