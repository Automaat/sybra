package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/Automaat/sybra/internal/llmjob"
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
type recoveryVerdict struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

// recoveryFenceRe tolerates up to three leading spaces on the fence so a
// declaration nested in a list item or blockquote still parses.
var recoveryFenceRe = regexp.MustCompile("(?ms)^[ \\t]{0,3}```[ \\t]*sybra-recovery[ \\t]*\\n(.*?)\\n[ \\t]{0,3}```")

// errNoRecoveryDeclaration means the run declared nothing. Recovery treats it
// as "leave the task parked", not as an error worth reporting.
var errNoRecoveryDeclaration = errors.New("recovery verdict: no declaration")

// parseRecoveryVerdict reads the verdict an implementation agent declared,
// either as a fenced sybra-recovery block or, on the status-reason path where
// the reason is a single-line CLI argument, as a bare JSON object.
//
// The last fenced block wins: the prompt asks for the declaration at the end
// of the response, and an agent that quotes the contract before answering
// would otherwise have its quotation read as the verdict.
func parseRecoveryVerdict(text string) (recoveryVerdict, error) {
	if payload, ok := lastFencedRecoveryPayload(text); ok {
		return decodeRecoveryVerdict(payload)
	}
	if obj := llmjob.ExtractLastJSONObject(text); obj != "" {
		var probe struct {
			Decision string `json:"decision"`
		}
		if err := json.Unmarshal([]byte(obj), &probe); err == nil && strings.TrimSpace(probe.Decision) != "" {
			return decodeRecoveryVerdict(obj)
		}
	}
	return recoveryVerdict{}, errNoRecoveryDeclaration
}

func lastFencedRecoveryPayload(text string) (string, bool) {
	matches := recoveryFenceRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return "", false
	}
	return matches[len(matches)-1][1], true
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
