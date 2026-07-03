package review

import (
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

func TestComputeReviewPhase(t *testing.T) {
	tests := []struct {
		name string
		sig  reviewSignals
		want reviewPhaseResult
	}{
		{
			name: "agent running trumps everything, leaves status untouched",
			sig:  reviewSignals{AgentRunning: true, HasDraft: true, Submitted: true},
			want: reviewPhaseResult{Phase: ReviewPhaseReviewing},
		},
		{
			name: "agent running trumps a conflicting PR",
			sig:  reviewSignals{AgentRunning: true, Mergeable: "CONFLICTING"},
			want: reviewPhaseResult{Phase: ReviewPhaseReviewing},
		},
		{
			name: "conflicting PR → conflict phase, passive in-review status",
			sig:  reviewSignals{Mergeable: "CONFLICTING"},
			want: reviewPhaseResult{Phase: ReviewPhaseConflict, Status: task.StatusInReview, Reason: "PR has merge conflicts — author must rebase"},
		},
		{
			name: "conflict outranks a pending draft / submitted review",
			sig:  reviewSignals{Mergeable: "CONFLICTING", HasDraft: true, Submitted: true, ViewerApproved: true},
			want: reviewPhaseResult{Phase: ReviewPhaseConflict, Status: task.StatusInReview, Reason: "PR has merge conflicts — author must rebase"},
		},
		{
			name: "UNKNOWN mergeable is not a conflict → normal awaiting-author",
			sig:  reviewSignals{Mergeable: "UNKNOWN", Submitted: true, HeadSHA: "sha1", ReviewedSHA: "sha1"},
			want: reviewPhaseResult{Phase: ReviewPhaseAwaitingAuthor, Status: task.StatusInReview, Reason: "Awaiting author response"},
		},
		{
			name: "approved waits for merge",
			sig:  reviewSignals{ViewerApproved: true, Submitted: true, HeadSHA: "abc"},
			want: reviewPhaseResult{Phase: ReviewPhaseApproved, Status: task.StatusInReview, Reason: "Approved — awaiting merge"},
		},
		{
			name: "pending draft → drafted, needs human to post",
			sig:  reviewSignals{HasDraft: true, ReRequested: true, HeadSHA: "abc"},
			want: reviewPhaseResult{Phase: ReviewPhaseDrafted, Status: task.StatusHumanRequired, Reason: "Draft review ready — verify & submit on GitHub"},
		},
		{
			name: "submitted at current head, not re-requested → awaiting author",
			sig:  reviewSignals{Submitted: true, HeadSHA: "sha1", ReviewedSHA: "sha1"},
			want: reviewPhaseResult{Phase: ReviewPhaseAwaitingAuthor, Status: task.StatusInReview, Reason: "Awaiting author response"},
		},
		{
			name: "submitted, head unknown → awaiting author (no false advance)",
			sig:  reviewSignals{Submitted: true, ReviewedSHA: "sha1"},
			want: reviewPhaseResult{Phase: ReviewPhaseAwaitingAuthor, Status: task.StatusInReview, Reason: "Awaiting author response"},
		},
		{
			name: "author pushed past reviewed commit → needs approval",
			sig:  reviewSignals{Submitted: true, HeadSHA: "sha2", ReviewedSHA: "sha1"},
			want: reviewPhaseResult{Phase: ReviewPhaseNeedsApproval, Status: task.StatusInReview, Reason: "Author updated PR — do a final review & approve"},
		},
		{
			name: "base-only merge commit after review → still awaiting author",
			sig:  reviewSignals{Submitted: true, HeadSHA: "merge", ReviewedSHA: "sha1", BaseOnlyMergeFromReviewed: true},
			want: reviewPhaseResult{Phase: ReviewPhaseAwaitingAuthor, Status: task.StatusInReview, Reason: "Awaiting author response"},
		},
		{
			name: "changed head with unknown lineage → fail closed awaiting author",
			sig:  reviewSignals{Submitted: true, HeadSHA: "sha2", ReviewedSHA: "sha1", HeadLineageUnknown: true},
			want: reviewPhaseResult{Phase: ReviewPhaseAwaitingAuthor, Status: task.StatusInReview, Reason: "Awaiting author response"},
		},
		{
			name: "merge commit after author fix → needs approval",
			sig:  reviewSignals{Submitted: true, HeadSHA: "merge", ReviewedSHA: "sha1"},
			want: reviewPhaseResult{Phase: ReviewPhaseNeedsApproval, Status: task.StatusInReview, Reason: "Author updated PR — do a final review & approve"},
		},
		{
			name: "re-requested after review → needs approval even at same head",
			sig:  reviewSignals{Submitted: true, ReRequested: true, HeadSHA: "sha1", ReviewedSHA: "sha1"},
			want: reviewPhaseResult{Phase: ReviewPhaseNeedsApproval, Status: task.StatusInReview, Reason: "Author updated PR — do a final review & approve"},
		},
		{
			name: "no agent, no draft, not submitted → manual (small-PR punt)",
			sig:  reviewSignals{ReRequested: true, HeadSHA: "sha1"},
			want: reviewPhaseResult{Phase: ReviewPhaseManual, Status: task.StatusHumanRequired},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeReviewPhase(tt.sig)
			if got != tt.want {
				t.Errorf("computeReviewPhase(%+v)\n got: %+v\nwant: %+v", tt.sig, got, tt.want)
			}
		})
	}
}

func TestStickyConflictPhase(t *testing.T) {
	tests := []struct {
		name         string
		mergeable    string
		currentPhase string
		wantDecided  bool
		wantPhase    string
	}{
		{"definitive conflict decides over any current phase", "CONFLICTING", ReviewPhaseDrafted, true, ReviewPhaseConflict},
		{"unknown holds an existing conflict", "UNKNOWN", ReviewPhaseConflict, true, ReviewPhaseConflict},
		{"empty holds an existing conflict", "", ReviewPhaseConflict, true, ReviewPhaseConflict},
		{"unknown does not invent a conflict", "UNKNOWN", ReviewPhaseAwaitingAuthor, false, ""},
		{"empty does not invent a conflict", "", ReviewPhaseDrafted, false, ""},
		{"mergeable clears a prior conflict", "MERGEABLE", ReviewPhaseConflict, false, ""},
		{"unexpected state holds an existing conflict", "SOME_NEW_STATE", ReviewPhaseConflict, true, ReviewPhaseConflict},
		{"unexpected state does not invent a conflict", "SOME_NEW_STATE", ReviewPhaseDrafted, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, decided := stickyConflictPhase(tt.mergeable, tt.currentPhase)
			if decided != tt.wantDecided {
				t.Fatalf("decided = %v, want %v", decided, tt.wantDecided)
			}
			if decided && res.Phase != tt.wantPhase {
				t.Errorf("phase = %q, want %q", res.Phase, tt.wantPhase)
			}
		})
	}
}

func TestReviewPhasePublished(t *testing.T) {
	tests := []struct {
		prev, next string
		want       bool
	}{
		{ReviewPhaseDrafted, ReviewPhaseAwaitingAuthor, true},
		{ReviewPhaseManual, ReviewPhaseAwaitingAuthor, true},
		{ReviewPhaseReviewing, ReviewPhaseNeedsApproval, true},
		{"", ReviewPhaseAwaitingAuthor, true},
		{ReviewPhaseAwaitingAuthor, ReviewPhaseNeedsApproval, false}, // already published
		{ReviewPhaseDrafted, ReviewPhaseApproved, false},             // approval, not a publish
		{ReviewPhaseAwaitingAuthor, ReviewPhaseApproved, false},
		{ReviewPhaseDrafted, ReviewPhaseManual, false},
	}
	for _, tt := range tests {
		if got := reviewPhasePublished(tt.prev, tt.next); got != tt.want {
			t.Errorf("reviewPhasePublished(%q, %q) = %v, want %v", tt.prev, tt.next, got, tt.want)
		}
	}
}
