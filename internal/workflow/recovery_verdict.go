package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Decisions an implementation agent may declare about closing its task
// without a PR.
const (
	recoveryDecisionAlreadyFixed = "already-fixed-on-main"
	recoveryDecisionNone         = "none"
)

// recoveryVerdict is the implementation role's structured statement about
// whether Sybra may close the task as an already-landed duplicate.
//
// Recovery reads this and nothing else. The predecessor scanned the run's
// prose for phrases like "already fixed" plus a word containing "main", which
// matched "main.go", "func main()" and "remaining", and had no notion of
// negation: a run reporting it had checked and the change was NOT on main
// closed the task as done.
//
// The agent declares it by setting the task's status reason to exactly this
// object, so a declaration costs a deliberate CLI call that narration cannot
// imitate.
type recoveryVerdict struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

// errNoRecoveryDeclaration means the run declared nothing. Recovery treats it
// as "leave the task parked", not as an error worth reporting.
var errNoRecoveryDeclaration = errors.New("recovery verdict: no declaration")

// parseRecoveryVerdict reads the verdict only when the whole signal is the
// declaration, which in practice means the agent set it as the task's status
// reason through the CLI.
//
// Nothing is extracted from a larger body of text. Every attempt to lift a
// declaration out of prose reopened the bug this replaces: an agent that
// quotes the contract to say it is NOT declaring a verdict had the quotation
// read as one, whether the quote was inline JSON, a fenced block, a markdown
// table, or an indented fence. Requiring the whole string removes the class
// rather than the last shape someone thought of.
func parseRecoveryVerdict(text string) (recoveryVerdict, error) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		return recoveryVerdict{}, errNoRecoveryDeclaration
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &probe); err != nil {
		return recoveryVerdict{}, fmt.Errorf("recovery verdict: malformed json: %w", err)
	}
	if _, ok := probe["decision"]; !ok {
		return recoveryVerdict{}, errNoRecoveryDeclaration
	}
	return decodeRecoveryVerdict(trimmed)
}

func decodeRecoveryVerdict(payload string) (recoveryVerdict, error) {
	var v recoveryVerdict
	if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &v); err != nil {
		return recoveryVerdict{}, fmt.Errorf("recovery verdict: malformed json: %w", err)
	}
	v.Decision = strings.ToLower(strings.TrimSpace(v.Decision))
	v.Reason = strings.TrimSpace(v.Reason)
	switch v.Decision {
	case recoveryDecisionAlreadyFixed, recoveryDecisionNone:
		return v, nil
	case "":
		return recoveryVerdict{}, errors.New("recovery verdict: missing decision")
	default:
		return recoveryVerdict{}, fmt.Errorf("recovery verdict: unknown decision %q", v.Decision)
	}
}

// declaresAlreadyFixedOnMain reports whether the run declared that the
// requested change is already on the base branch.
//
// ok=false with a nil error means no declaration was made, so the task stays
// parked for a human. A non-nil error means a declaration was present and
// unreadable, which the caller records on the task.
func declaresAlreadyFixedOnMain(signal string) (alreadyFixed, ok bool, err error) {
	v, err := parseRecoveryVerdict(signal)
	if errors.Is(err, errNoRecoveryDeclaration) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return v.Decision == recoveryDecisionAlreadyFixed, true, nil
}

// recoveryVerdictReason returns the agent's own justification for a declared
// verdict, so a task closed as a duplicate records why rather than only that
// it happened.
func recoveryVerdictReason(signal string) string {
	v, err := parseRecoveryVerdict(signal)
	if err != nil {
		return ""
	}
	return v.Reason
}
