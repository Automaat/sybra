package github

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestSearchQueriesFollowCursor(t *testing.T) {
	t.Parallel()
	issuePage := func(number int, more bool, cursor string) []byte {
		return []byte(`{"data":{"search":{"pageInfo":{"hasNextPage":` + boolJSON(more) + `,"endCursor":"` + cursor + `"},"nodes":[{"number":` + itoa(number) + `,"title":"issue","url":"https://github.com/o/r/issues/` + itoa(number) + `","repository":{"name":"r","nameWithOwner":"o/r"},"labels":{"nodes":[]},"author":{"login":"dev"}}]}}}`)
	}
	prPage := func(number int, author string, more bool, cursor string) []byte {
		return []byte(`{"data":{"viewer":{"login":"me"},"search":{"pageInfo":{"hasNextPage":` + boolJSON(more) + `,"endCursor":"` + cursor + `"},"nodes":[{"number":` + itoa(number) + `,"title":"pr","url":"https://github.com/o/r/pull/` + itoa(number) + `","author":{"login":"` + author + `","type":"User"},"repository":{"name":"r","nameWithOwner":"o/r"},"labels":{"nodes":[]},"commits":{"nodes":[]},"reviewThreads":{"nodes":[]}}]}}}`)
	}

	tests := []struct {
		name string
		run  func(*sequenceExecer) (int, error)
		one  []byte
		two  []byte
	}{
		{
			name: "issues",
			run: func(e *sequenceExecer) (int, error) {
				out, err := searchIssuesWith(e, "is:issue")
				return len(out), err
			},
			one: issuePage(1, true, "cursor-1"), two: issuePage(2, false, ""),
		},
		{
			name: "pull requests",
			run: func(e *sequenceExecer) (int, error) {
				out, err := searchPRsWith(e, "is:pr")
				return len(out), err
			},
			one: prPage(1, "dev", true, "cursor-1"), two: prPage(2, "dev", false, ""),
		},
		{
			name: "review summary",
			run: func(e *sequenceExecer) (int, error) {
				out, err := fetchReviewSearchWith(e, "is:pr")
				return len(out), err
			},
			one: prPage(1, "dev", true, "cursor-1"), two: prPage(2, "dev", false, ""),
		},
		{
			name: "renovate",
			run: func(e *sequenceExecer) (int, error) {
				out, err := searchRenovatePRsWith(context.Background(), e, "is:pr")
				return len(out), err
			},
			one: prPage(1, "renovate[bot]", true, "cursor-1"), two: prPage(2, "renovate[bot]", false, ""),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &sequenceExecer{outputs: [][]byte{tt.one, tt.two}}
			got, err := tt.run(e)
			if err != nil {
				t.Fatalf("search: %v", err)
				panic("unreachable")
			}
			if got != 2 || e.calls != 2 {
				t.Fatalf("results/calls = %d/%d, want 2/2", got, e.calls)
			}
			if !hasPaginationArg(e.args[1], "after=cursor-1") {
				t.Fatalf("second request did not send cursor: %q", e.args[1])
			}
		})
	}
}

func TestSearchQueriesRejectMissingNextCursor(t *testing.T) {
	t.Parallel()
	e := &fakeExecer{output: []byte(`{"data":{"search":{"pageInfo":{"hasNextPage":true,"endCursor":""},"nodes":[]}}}`)}
	if _, err := searchIssuesWith(e, "is:issue"); err == nil || !strings.Contains(err.Error(), "without cursor") {
		t.Fatalf("error = %v, want missing cursor refusal", err)
		panic("unreachable")
	}
}

func hasPaginationArg(args []string, want string) bool {
	return slices.Contains(args, want)
}

func boolJSON(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func itoa(v int) string {
	return strconv.Itoa(v)
}
