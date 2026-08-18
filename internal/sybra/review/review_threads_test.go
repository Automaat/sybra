package review

import (
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/workflow"

	"context"
)

func TestReviewThreadLocation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		thread github.ReviewThread
		want   string
	}{
		{
			name:   "path and line name the anchor",
			thread: github.ReviewThread{Path: "internal/a.go", Line: 12, AuthorLogin: "reviewer"},
			want:   "internal/a.go:12 by reviewer",
		},
		{
			name:   "a lost anchor keeps the file",
			thread: github.ReviewThread{Path: "internal/a.go", AuthorLogin: "reviewer"},
			want:   "internal/a.go by reviewer",
		},
		{
			name:   "a file-level thread is still nameable",
			thread: github.ReviewThread{AuthorLogin: "reviewer"},
			want:   "(PR-level) by reviewer",
		},
		{
			name:   "a missing author drops the suffix rather than printing an empty one",
			thread: github.ReviewThread{Path: "internal/a.go", Line: 3},
			want:   "internal/a.go:3",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := reviewThreadLocation(tc.thread); got != tc.want {
				t.Errorf("reviewThreadLocation = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReviewThreadBriefVars(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		brief   reviewThreadBrief
		wantVar bool
	}{
		{name: "an empty brief writes no variable, so the step skips"},
		{
			name: "a populated brief round-trips through the variable",
			brief: reviewThreadBrief{threads: []workflow.BriefedReviewThread{
				{ID: "t1", LastAuthor: "reviewer"},
			}},
			wantVar: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw := tc.brief.vars()
			if (raw != "") != tc.wantVar {
				t.Fatalf("vars() = %q, want non-empty=%v", raw, tc.wantVar)
			}
			if tc.wantVar && len(workflow.UnmarshalBriefedReviewThreads(raw)) != len(tc.brief.threads) {
				t.Errorf("round trip lost threads: %q", raw)
			}
		})
	}
}

// The brief is only worth carrying if the agent is told the count binds it.
// A list the agent may silently under-read is the defect this replaces.
func TestCommentsPromptCarriesTheBrief(t *testing.T) {
	t.Parallel()

	pr := github.PullRequest{Number: 7, Repository: "o/r", HeadRefName: "feat", URL: "https://github.com/o/r/pull/7"}

	bare := commentsPrompt(context.Background(), pr, project.SigningAuto, reviewThreadBrief{})
	if strings.Contains(bare, "harness counted") {
		t.Errorf("empty brief must add nothing to the prompt:\n%s", bare)
	}

	brief := reviewThreadBrief{prompt: "The harness counted 2 unresolved review thread(s) on this PR:\n- internal/a.go:12 by reviewer"}
	withBrief := commentsPrompt(context.Background(), pr, project.SigningAuto, brief)
	for _, want := range []string{"harness counted 2", "internal/a.go:12", "/fix-review"} {
		if !strings.Contains(withBrief, want) {
			t.Errorf("prompt missing %q:\n%s", want, withBrief)
		}
	}
}
