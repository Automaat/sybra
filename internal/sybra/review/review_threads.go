package review

import (
	"context"
	"fmt"
	"strings"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/workflow"
)

// reviewThreadBrief is the Go-fetched ground truth about a PR's unresolved
// review threads. The agent enumerates threads itself inside /fix-review, so
// this exists to bound that enumeration from outside: the prompt states how
// many threads the harness sees, and verify_review_threads later re-checks the
// same set. Without it an agent whose own fetch fails answers a fraction of
// the feedback and still reports success.
type reviewThreadBrief struct {
	threads []workflow.BriefedReviewThread
	prompt  string
}

// vars returns the workflow variable value carrying this brief, empty when
// there is nothing to verify.
func (b reviewThreadBrief) vars() string {
	return workflow.MarshalBriefedReviewThreads(b.threads)
}

// fetchReviewThreadBrief loads the PR's unresolved review threads. A fetch
// failure yields an empty brief and a prompt line saying so: the agent is told
// to trust its own enumeration in that case, and verify_review_threads has
// nothing to hold it to, which is the same position the code was in before
// this brief existed.
func fetchReviewThreadBrief(ctx context.Context, pr github.PullRequest) reviewThreadBrief {
	all, err := github.FetchReviewThreadsContext(ctx, pr.Repository, pr.Number)
	if err != nil {
		return reviewThreadBrief{}
	}

	var (
		brief reviewThreadBrief
		locs  []string
	)
	for i := range all {
		if all[i].IsResolved || all[i].IsOutdated {
			continue
		}
		brief.threads = append(brief.threads, workflow.BriefedReviewThread{
			ID:         all[i].ID,
			LastAuthor: all[i].LastAuthorLogin,
		})
		locs = append(locs, reviewThreadLocation(all[i]))
	}
	if len(brief.threads) == 0 {
		return brief
	}

	var b strings.Builder
	fmt.Fprintf(&b, "The harness counted %d unresolved review thread(s) on this PR:\n", len(locs))
	for _, loc := range locs {
		b.WriteString("- ")
		b.WriteString(loc)
		b.WriteByte('\n')
	}
	b.WriteString("\nThat count is authoritative. If your own enumeration finds fewer " +
		"threads than this, your fetch failed — retry it, and report failure rather " +
		"than success if you still cannot read them all. Answering only the threads " +
		"you managed to fetch and reporting success drops the rest of the reviewer's " +
		"feedback silently.")
	brief.prompt = b.String()
	return brief
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
