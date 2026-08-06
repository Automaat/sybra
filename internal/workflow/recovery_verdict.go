package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Decisions an implementation agent may declare about closing its task
// without a PR.
const (
	recoveryDecisionAlreadyFixed = "already-fixed-on-main"
	recoveryDecisionNone         = "none"
)

// recoveryVerdict is the implementation role's structured statement about
// whether Sybra may close the task as an already-landed duplicate. It exists
// so the close decision reads a value the agent declared rather than a
// substring of whatever prose the run happened to emit.
type recoveryVerdict struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

var recoveryFenceRe = regexp.MustCompile("(?s)```\\s*sybra-recovery\\s*\\n(.*?)\\n```")

// Legacy prose signals, kept for agents whose prompt predates the declaration
// block. Every branch token is word-anchored: an unanchored "main" also
// matches "remaining", "maintain" and "domain", which made the old base-branch
// signal true for almost any English sentence and left the phrase set below as
// the only real gate.
var (
	fixedPhraseRe = regexp.MustCompile(`already (fixed|on main|merged|landed|satisfied)|duplicate task`)
	basePhraseRe  = regexp.MustCompile(`\b(main|upstream|origin)\b`)
	closePhraseRe = regexp.MustCompile(`no pr (needed|required)|safe to close|mark (as )?done`)
)

// parseRecoveryVerdict returns the verdict an implementation agent declared in
// a fenced ```sybra-recovery``` block. declared=false means the run made no
// declaration at all, which is the normal case for an agent on an older
// prompt. A block that is present but unreadable returns an error instead:
// once an agent has tried to declare and failed, falling back to guessing from
// its prose is the behaviour this replaces.
func parseRecoveryVerdict(text string) (v recoveryVerdict, declared bool, err error) {
	m := recoveryFenceRe.FindStringSubmatch(text)
	if len(m) < 2 {
		return recoveryVerdict{}, false, nil
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(m[1])), &v); err != nil {
		return recoveryVerdict{}, false, fmt.Errorf("recovery verdict: malformed json: %w", err)
	}
	v.Decision = strings.ToLower(strings.TrimSpace(v.Decision))
	v.Reason = strings.TrimSpace(v.Reason)
	switch v.Decision {
	case recoveryDecisionAlreadyFixed, recoveryDecisionNone:
		return v, true, nil
	case "":
		return recoveryVerdict{}, false, errors.New("recovery verdict: missing decision")
	default:
		return recoveryVerdict{}, false, fmt.Errorf("recovery verdict: unknown decision %q", v.Decision)
	}
}

// declaresAlreadyFixedOnMain reports whether signal says the requested change
// is already on the base branch. A structured declaration is authoritative. In
// its absence the legacy prose match decides, and an unreadable declaration
// decides nothing at all, so the caller leaves the task parked.
func declaresAlreadyFixedOnMain(signal string) (bool, error) {
	v, declared, err := parseRecoveryVerdict(signal)
	if err != nil {
		return false, err
	}
	if declared {
		return v.Decision == recoveryDecisionAlreadyFixed, nil
	}
	return looksLikeAlreadyFixedOnMainProse(signal), nil
}

// looksLikeAlreadyFixedOnMainProse is the legacy heuristic: an explicit
// already-landed phrase, corroborated by either a base-branch mention or an
// explicit request to close. It is deliberately narrower than a bare keyword
// scan and is only reached when the run declared nothing.
func looksLikeAlreadyFixedOnMainProse(signal string) bool {
	lower := strings.ToLower(strings.TrimSpace(signal))
	if lower == "" {
		return false
	}
	if !fixedPhraseRe.MatchString(lower) {
		return false
	}
	return basePhraseRe.MatchString(lower) || closePhraseRe.MatchString(lower)
}
