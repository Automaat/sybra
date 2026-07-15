package github

import (
	"context"
	"strings"
	"testing"
)

func TestParseCommitMessages(t *testing.T) {
	out := []byte("feat: a\n\nThis reverts commit abc.\nfix: b\n  \n")
	got := parseCommitMessages(out)
	want := []string{"feat: a", "This reverts commit abc.", "fix: b"}
	if len(got) != len(want) {
		t.Fatalf("got %d msgs %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("msg[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseCommitMessages_Empty(t *testing.T) {
	if got := parseCommitMessages([]byte("")); len(got) != 0 {
		t.Errorf("empty output should yield no messages, got %v", got)
	}
}

func TestFetchCommitParentSHAsWith(t *testing.T) {
	t.Parallel()
	fe := &recordingExecer{output: []byte("parent1\nparent2\n")}
	sha := "0123456789abcdef0123456789abcdef01234567"

	got, err := fetchCommitParentSHAsWith(context.Background(), fe, "o/r", sha)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 || got[0] != "parent1" || got[1] != "parent2" {
		t.Fatalf("parents = %v, want [parent1 parent2]", got)
	}
	wantPath := "repos/o/r/commits/" + sha
	foundPath := false
	foundJQ := false
	for _, arg := range fe.lastArgs {
		if arg == wantPath {
			foundPath = true
		}
		if arg == ".parents[].sha" {
			foundJQ = true
		}
	}
	if !foundPath || !foundJQ {
		t.Fatalf("args = %v, want commit endpoint and parent jq", fe.lastArgs)
	}
}

func TestFetchCommitParentSHAsWith_RejectsNonSHA(t *testing.T) {
	t.Parallel()
	fe := &recordingExecer{output: []byte("parent1\nparent2\n")}

	_, err := fetchCommitParentSHAsWith(context.Background(), fe, "o/r", "main")
	if err == nil || !strings.Contains(err.Error(), "invalid commit sha") {
		t.Fatalf("err = %v, want invalid commit sha", err)
	}
	if fe.calls != 0 {
		t.Fatalf("calls = %d, want 0", fe.calls)
	}
}

func TestFetchCommitParentSHAsWith_UsesContext(t *testing.T) {
	t.Parallel()
	sha := "0123456789abcdef0123456789abcdef01234567"
	fe := &ctxFakeExecer{output: []byte("parent1\nparent2\n")}

	if _, err := fetchCommitParentSHAsWith(context.Background(), fe, "o/r", sha); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !fe.usedCtx {
		t.Fatal("expected the context-aware runCtx path to be used")
	}
}

func TestIsBaseOnlyMergeFromReviewed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name              string
		parents           []string
		reviewedSHA       string
		baseCompareStatus string
		want              bool
	}{
		{
			name:              "direct merge of reachable base into reviewed commit",
			parents:           []string{"reviewed", "base-parent"},
			reviewedSHA:       "reviewed",
			baseCompareStatus: "ahead",
			want:              true,
		},
		{
			name:              "base parent is current base",
			parents:           []string{"reviewed", "base-parent"},
			reviewedSHA:       "reviewed",
			baseCompareStatus: "identical",
			want:              true,
		},
		{
			name:              "second parent is not reachable from base",
			parents:           []string{"reviewed", "feature-parent"},
			reviewedSHA:       "reviewed",
			baseCompareStatus: "diverged",
			want:              false,
		},
		{
			name:              "first parent is not the reviewed commit",
			parents:           []string{"author-fix", "base-parent"},
			reviewedSHA:       "reviewed",
			baseCompareStatus: "ahead",
			want:              false,
		},
		{
			name:              "not a merge commit",
			parents:           []string{"reviewed"},
			reviewedSHA:       "reviewed",
			baseCompareStatus: "ahead",
			want:              false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isBaseOnlyMergeFromReviewed(tt.parents, tt.reviewedSHA, tt.baseCompareStatus)
			if got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseCommitParentSHAs(t *testing.T) {
	got := parseCommitParentSHAs([]byte(" parent1 \n\nparent2\n"))
	if len(got) != 2 || got[0] != "parent1" || got[1] != "parent2" {
		t.Fatalf("parents = %v, want [parent1 parent2]", got)
	}
}
