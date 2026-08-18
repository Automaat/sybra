package workflow

import "encoding/json"

// PRReviewThreadBriefVar names the workflow variable carrying the unresolved
// review threads a pr-fix run was briefed on, as written by the review
// dispatcher and read back by the verify_review_threads step.
const PRReviewThreadBriefVar = "pr_review_thread_brief"

// BriefedReviewThread records one unresolved review thread as it looked when
// the pr-fix agent was briefed on it.
//
// LastAuthor is the identity check. The harness cannot ask "did the agent
// answer this thread?" by resolution state alone: a reviewer's thread stays
// unresolved after a correct reply, and the account the agent posts as varies
// by deployment (a GitHub App installation token reports no viewer login at
// all). Comparing the thread's newest comment author against the author
// recorded here needs no identity lookup: any reply, by anyone, moves it.
type BriefedReviewThread struct {
	ID         string `json:"id"`
	LastAuthor string `json:"last_author"`
}

// MarshalBriefedReviewThreads encodes threads for the workflow variable.
// Returns "" for an empty set so the step's skip check stays a plain
// emptiness test.
func MarshalBriefedReviewThreads(threads []BriefedReviewThread) string {
	if len(threads) == 0 {
		return ""
	}
	b, err := json.Marshal(threads)
	if err != nil {
		return ""
	}
	return string(b)
}

// UnmarshalBriefedReviewThreads decodes the workflow variable. A malformed or
// empty value yields no threads, which the step treats as nothing to verify.
func UnmarshalBriefedReviewThreads(raw string) []BriefedReviewThread {
	if raw == "" {
		return nil
	}
	var threads []BriefedReviewThread
	if err := json.Unmarshal([]byte(raw), &threads); err != nil {
		return nil
	}
	return threads
}
