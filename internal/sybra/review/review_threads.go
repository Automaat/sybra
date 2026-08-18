package review

import (
	"context"
	"fmt"
	"strings"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/workflow"
)

// reviewThreadBrief is the Go-fetched ground truth about the review threads a
// pr-fix run is expected to answer. The agent enumerates threads itself inside
// /fix-review, so this exists to bound that enumeration from outside: the
// prompt states how many threads the harness sees, and verify_review_threads
// later re-checks the same set. Without it an agent whose own fetch fails
// answers a fraction of the feedback and still reports success.
type reviewThreadBrief struct {
	threads []workflow.BriefedReviewThread
	prompt  string
}

// vars returns the workflow variable value carrying this brief, empty when
// there is nothing to verify.
func (b reviewThreadBrief) vars() string {
	return workflow.MarshalBriefedReviewThreads(b.threads)
}

// fetchReviewThreadBrief loads the threads this run owes a reply to. It must
// match the monitor's own actionable rule (FetchPRReviewState): a thread the
// agent already answered stays unresolved forever, because the fix-review
// skill never resolves a reviewer's thread and the reviewer rarely does
// either. Briefing those would park every later run on a thread no reply can
// ever clear.
//
// A fetch failure yields an empty brief, which leaves the run exactly where it
// stood before this brief existed: the agent trusts its own enumeration and
// verify_review_threads holds it to nothing.
func fetchReviewThreadBrief(ctx context.Context, pr github.PullRequest, agentLogin string) reviewThreadBrief {
	all, err := github.FetchReviewThreadsContext(ctx, pr.Repository, pr.Number)
	if err != nil {
		return reviewThreadBrief{}
	}

	var (
		brief reviewThreadBrief
		locs  []string
	)
	for i := range all {
		if !actionableReviewThread(all[i], agentLogin) {
			continue
		}
		brief.threads = append(brief.threads, workflow.BriefedReviewThread{
			ID:       all[i].ID,
			Comments: all[i].CommentCount,
		})
		locs = append(locs, reviewThreadLocation(all[i]))
	}
	if len(brief.threads) == 0 {
		return brief
	}

	var b strings.Builder
	fmt.Fprintf(&b, "The harness counted %d review thread(s) on this PR still waiting on a reply from you:\n", len(locs))
	for _, loc := range locs {
		b.WriteString("- ")
		b.WriteString(loc)
		b.WriteByte('\n')
	}
	b.WriteString("\nThat count is authoritative. If your own enumeration finds fewer " +
		"threads than this, your fetch failed - retry it, and report failure rather " +
		"than success if you still cannot read them all. Answering only the threads " +
		"you managed to fetch and reporting success drops the rest of the reviewer's " +
		"feedback silently.")
	brief.prompt = b.String()
	return brief
}

// actionableReviewThread reports whether a thread is still waiting on the
// harness. Mirrors the actionable rule in github.FetchPRReviewState: a
// reviewer had the last word, and the thread is neither resolved nor anchored
// to code that has since moved.
func actionableReviewThread(t github.ReviewThread, agentLogin string) bool {
	if t.IsResolved || t.IsOutdated {
		return false
	}
	if t.LastAuthorLogin == "" {
		return false
	}
	return !strings.EqualFold(t.LastAuthorLogin, agentLogin)
}

// reviewThreadLocation names one thread for the prompt. Threads carry no
// stable human-facing handle, so path and line are what let the agent tell the
// harness's list apart from its own.
func reviewThreadLocation(t github.ReviewThread) string {
	loc := t.Path
	if loc == "" {
		loc = "(PR-level)"
	} else if t.Line > 0 {
		loc = fmt.Sprintf("%s:%d", t.Path, t.Line)
	}
	if t.AuthorLogin != "" {
		loc += " by " + t.AuthorLogin
	}
	return loc
}
