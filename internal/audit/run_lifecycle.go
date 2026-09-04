package audit

import (
	"fmt"
	"time"

	"github.com/Automaat/sybra/internal/runoutcome"
)

const (
	RunCompatCanonical             = "canonical"
	RunCompatLegacyFailed          = "legacy_failed"
	RunCompatLegacyFailedShadowed  = "legacy_failed_shadowed"
	RunCompatMissingOutcomeUnknown = "missing_outcome_unknown"
)

// RunLifecycle normalizes the audit-visible lifecycle of one agent run.
// Historical logs may encode a failed run as either agent.completed with a
// non-stopped state or a legacy agent.failed event; NormalizeAgentRuns dedupes
// those into one terminal run.
type RunLifecycle struct {
	Key           string
	AgentID       string
	TaskID        string
	StartedAt     time.Time
	TerminalAt    time.Time
	Started       bool
	Terminal      bool
	Failed        bool
	Outcome       string
	Reattached    bool
	Lost          bool
	Compatibility string
	TerminalEvent Event

	sawLegacyFailed bool
}

// NormalizeAgentRuns collapses raw audit lifecycle events into one record per
// run. It dedupes compatibility agent.failed events and exposes partial-window
// shapes: a terminal without a start is marked Reattached, and a start without
// a terminal is marked Lost.
func NormalizeAgentRuns(events []Event) []RunLifecycle {
	runs := map[string]*RunLifecycle{}
	order := make([]string, 0, len(events))

	for i := range events {
		e := events[i]
		start, terminal, outcome, ok := classifyRunEvent(e)
		if !ok {
			continue
		}

		key := runKey(e, i)
		run := runs[key]
		if run == nil {
			run = &RunLifecycle{
				Key:           key,
				AgentID:       e.AgentID,
				TaskID:        e.TaskID,
				Compatibility: RunCompatCanonical,
			}
			runs[key] = run
			order = append(order, key)
		}
		if run.AgentID == "" {
			run.AgentID = e.AgentID
		}
		if run.TaskID == "" {
			run.TaskID = e.TaskID
		}

		if start {
			run.Started = true
			if run.StartedAt.IsZero() || e.Timestamp.Before(run.StartedAt) {
				run.StartedAt = e.Timestamp
			}
		}

		if !terminal {
			continue
		}

		run.Terminal = true
		run.sawLegacyFailed = run.sawLegacyFailed || e.Type == EventAgentFailed
		if shouldReplaceTerminal(run.TerminalEvent, e) {
			run.TerminalEvent = e
			run.TerminalAt = e.Timestamp
			run.Outcome = outcome
			run.Failed = outcome == runoutcome.Failed
		} else if run.TerminalAt.IsZero() {
			run.TerminalAt = e.Timestamp
		}
	}

	out := make([]RunLifecycle, 0, len(order))
	for _, key := range order {
		runp := runs[key]
		if runp == nil {
			continue
		}
		run := *runp
		run.Reattached = run.Terminal && !run.Started
		run.Lost = run.Started && !run.Terminal
		run.Compatibility = runCompatibility(run)
		out = append(out, run)
	}
	return out
}

func runKey(e Event, index int) string {
	if e.AgentID != "" {
		return "agent:" + e.AgentID
	}
	return fmt.Sprintf("event:%d:%s:%s:%d", index, e.Type, e.TaskID, e.Timestamp.UnixNano())
}

func classifyRunEvent(e Event) (start, terminal bool, outcome string, ok bool) {
	switch e.Type {
	case EventAgentStarted:
		return true, false, runoutcome.Started, true
	case EventAgentFailed:
		return false, true, runoutcome.Failed, true
	case EventAgentCompleted:
		return false, true, classifyCompletedOutcome(e), true
	default:
		return false, false, "", false
	}
}

func classifyCompletedOutcome(e Event) string {
	if outcome, ok := e.Data["outcome"].(string); ok && outcome != "" {
		return runoutcome.Normalize(outcome)
	}
	state, hasState := e.Data["state"].(string)
	switch {
	case !hasState || state == "":
		return runoutcome.Unknown
	case state == "stopped":
		return runoutcome.Completed
	default:
		return runoutcome.Failed
	}
}

func shouldReplaceTerminal(cur, next Event) bool {
	if cur.Type == "" {
		return true
	}
	if cur.Type != EventAgentCompleted && next.Type == EventAgentCompleted {
		return true
	}
	if cur.Type == EventAgentCompleted && next.Type != EventAgentCompleted {
		return false
	}

	curState, curHasState := cur.Data["state"].(string)
	nextState, nextHasState := next.Data["state"].(string)
	if (!curHasState || curState == "") && nextHasState && nextState != "" {
		return true
	}
	if curHasState && curState != "" && (!nextHasState || nextState == "") {
		return false
	}
	return next.Timestamp.After(cur.Timestamp)
}

func runCompatibility(run RunLifecycle) string {
	switch {
	case run.sawLegacyFailed && run.TerminalEvent.Type == EventAgentCompleted:
		return RunCompatLegacyFailedShadowed
	case run.sawLegacyFailed:
		return RunCompatLegacyFailed
	case run.TerminalEvent.Type == EventAgentCompleted:
		if outcome, _ := run.TerminalEvent.Data["outcome"].(string); outcome != "" {
			return RunCompatCanonical
		}
		state, _ := run.TerminalEvent.Data["state"].(string)
		if state == "" {
			return RunCompatMissingOutcomeUnknown
		}
		return RunCompatCanonical
	default:
		return RunCompatCanonical
	}
}
