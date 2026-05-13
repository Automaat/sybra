package github

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestFetchIssueLinkedPRsWith(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		output  string
		execErr error
		want    []PullRequest
		wantErr string
	}{
		{
			name: "one same-repo open PR",
			output: linkedPRsResponse(`[
				{"willCloseTarget":true,"source":{"number":7,"title":"fix bug","url":"https://github.com/owner/repo/pull/7","state":"OPEN","headRefName":"fix/bug","author":{"login":"me","type":"User"},"repository":{"name":"repo","nameWithOwner":"owner/repo"}}}
			]`),
			want: []PullRequest{{
				Number:      7,
				Title:       "fix bug",
				URL:         "https://github.com/owner/repo/pull/7",
				HeadRefName: "fix/bug",
				Repository:  "owner/repo",
				RepoName:    "repo",
				Author:      "me",
			}},
		},
		{
			name: "cross-repo ignored",
			output: linkedPRsResponse(`[
				{"willCloseTarget":true,"source":{"number":8,"title":"x","url":"https://github.com/else/repo/pull/8","state":"OPEN","headRefName":"x","author":{"login":"me","type":"User"},"repository":{"name":"repo","nameWithOwner":"else/repo"}}}
			]`),
			want: []PullRequest{},
		},
		{
			name: "closed PR ignored",
			output: linkedPRsResponse(`[
				{"willCloseTarget":true,"source":{"number":9,"title":"closed","url":"https://github.com/owner/repo/pull/9","state":"CLOSED","headRefName":"closed","author":{"login":"me","type":"User"},"repository":{"name":"repo","nameWithOwner":"owner/repo"}}}
			]`),
			want: []PullRequest{},
		},
		{
			name: "multiple PRs returned",
			output: linkedPRsResponse(`[
				{"willCloseTarget":true,"source":{"number":10,"title":"one","url":"https://github.com/owner/repo/pull/10","state":"OPEN","headRefName":"one","author":{"login":"me","type":"User"},"repository":{"name":"repo","nameWithOwner":"owner/repo"}}},
				{"willCloseTarget":true,"source":{"number":11,"title":"two","url":"https://github.com/owner/repo/pull/11","state":"OPEN","headRefName":"two","author":{"login":"me","type":"User"},"repository":{"name":"repo","nameWithOwner":"owner/repo"}}}
			]`),
			want: []PullRequest{
				{Number: 10, Title: "one", URL: "https://github.com/owner/repo/pull/10", HeadRefName: "one", Repository: "owner/repo", RepoName: "repo", Author: "me"},
				{Number: 11, Title: "two", URL: "https://github.com/owner/repo/pull/11", HeadRefName: "two", Repository: "owner/repo", RepoName: "repo", Author: "me"},
			},
		},
		{
			name:    "malformed JSON",
			output:  "not json",
			wantErr: "parse graphql response",
		},
		{
			name:    "graphql error",
			output:  `{"errors":[{"message":"bad query"}]}`,
			wantErr: "graphql: bad query",
		},
		{
			name:    "exec error",
			output:  "gh: bad credentials",
			execErr: fmt.Errorf("exit 1"),
			wantErr: "gh api graphql",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fe := &fakeExecer{output: []byte(tt.output), err: tt.execErr}
			got, err := fetchIssueLinkedPRsWith(fe, "owner/repo", 3)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d: %+v", len(got), len(tt.want), got)
			}
			for i := range got {
				if !reflect.DeepEqual(got[i], tt.want[i]) {
					t.Fatalf("pr[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func linkedPRsResponse(nodes string) string {
	return `{"data":{"repository":{"issue":{"timelineItems":{"nodes":` + nodes + `}}}}}`
}
