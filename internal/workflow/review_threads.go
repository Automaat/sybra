package workflow

import "encoding/json"

// PRReviewThreadBriefVar names the workflow variable carrying the unresolved
// review threads a pr-fix run was briefed on, as written by the review
// dispatcher and read back by the verify_review_threads step.
const PRReviewThreadBriefVar = "pr_review_thread_brief"

// BriefedReviewThread records one actionable review thread as it looked when
// the pr-fix agent was briefed on it.
//
// Comments is the identity check, and it is a count rather than an author for
// two reasons. The harness cannot ask "did the agent answer this?" by
// resolution state: the fix-review skill deliberately never resolves a
// reviewer's thread, so a correctly answered thread stays unresolved. It also
// cannot key on the newest comment's author: the reviewer may post again
// between the reply and this check, which would restore the brief-time author
// and read as though the agent had never replied. A comment count only ever
// grows, so any reply — by anyone — clears the thread.
type BriefedReviewThread struct {
	ID       string `json:"id"`
	Comments int    `json:"comments"`
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
