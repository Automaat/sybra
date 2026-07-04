package config

// ReviewHoldConfig gates whether Sybra may publish PR comment replies without a
// human check. When enabled, fix-review agents draft the replies they would
// have posted into a single PENDING (draft) pull-request review instead of
// posting live thread replies, never submit it, and the task is parked in
// human-required for the user to verify and submit on GitHub. Inbound reviews of
// a teammate's PR already work this way (a pending review + human-required); this
// extends the same gate to the reply/fix-review flow.
//
// Mode controls how far the hold extends to the agent's own code changes.
// Per-machine toggle: enable on the machine where you review before publishing.
type ReviewHoldConfig struct {
	// Enabled turns the hold on. Default off — preserves the live-reply behavior.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Mode controls what the fix-review agent may push once its replies are held:
	//   push       — reply held as a pending review; code fixes still committed
	//                and pushed to the PR branch (default).
	//   push_nits  — reply held; code pushed only when the diff is at most
	//                NitMaxLines changed lines (a nit), otherwise held for review.
	//   hold       — reply held AND code held; nothing is pushed, the human
	//                reviews the diff too. In a coalesced fix this also holds
	//                bundled non-reply changes (e.g. a CI fix), so CI stays red
	//                until the human pushes — the "review everything" tradeoff.
	// Unknown/empty values fall back to "push".
	Mode string `yaml:"mode" json:"mode"`
	// NitMaxLines is the changed-line ceiling that still counts as a "nit" for
	// the push_nits mode. Zero falls back to DefaultReviewHoldNitMaxLines.
	NitMaxLines int `yaml:"nit_max_lines" json:"nitMaxLines"`
}
