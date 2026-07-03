package sybra

import (
	"fmt"

	"github.com/Automaat/sybra/internal/config"
)

// reviewHoldFixSuffix returns the instruction block appended to a fix-review
// agent's prompt when the review-hold setting is on. It converts the agent's
// default "reply live on every thread and push" behavior into "draft the replies
// into one PENDING review, don't submit, and park the task for a human". The
// push clause varies by mode. Returns "" when the hold is disabled, so prompts
// stay byte-for-byte unchanged in the common case. Nil-safe (nil cfg = disabled).
func reviewHoldFixSuffix(cfg *config.Config) string {
	if !cfg.ReviewHoldEnabled() {
		return ""
	}

	var push string
	switch cfg.ReviewHoldMode() {
	case config.ReviewHoldModeHold:
		// hold also holds any non-reply code fix in a coalesced set (e.g. a
		// bundled CI fix), so CI stays red until a human pushes — the intended
		// "human reviews everything" tradeoff.
		push = "- Do NOT push anything — not even a bundled CI or conflict fix. Commit your " +
			"changes locally only (or leave them uncommitted) so a human reviews the whole " +
			"diff before it reaches the PR branch."
	case config.ReviewHoldModePushNits:
		push = fmt.Sprintf("- Push your code fixes ONLY if the whole diff is at most %d "+
			"changed lines (a nit). If it is larger, commit locally but do NOT push — "+
			"leave the branch for a human to review.", cfg.ReviewHoldNitMaxLines())
	default: // ReviewHoldModePush
		push = "- Push your code fixes to the PR branch as usual."
	}

	return "\n\n---\n" +
		"REVIEW HOLD is enabled — do NOT publish anything to the PR without a human check.\n" +
		"- Do NOT post live thread replies. Do NOT run `gh pr review`, and never submit, " +
		"approve, or request changes.\n" +
		"- Instead, collect every reply you would have posted and attach them to a SINGLE " +
		"PENDING (draft) pull-request review, left UNSUBMITTED — create the pending review " +
		"and add each thread reply to it (e.g. `gh api graphql` `addPullRequestReview` with " +
		"no event, then `addPullRequestReviewComment` with `inReplyTo` per thread). Leave it pending.\n" +
		push + "\n" +
		"- Sybra parks the task in human-required automatically; do not re-request review, " +
		"submit the review, or merge. A human verifies the pending review and submits it."
}
