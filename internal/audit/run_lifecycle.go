package audit

import (
	"fmt"
	"time"
)

const (
	RunCompatCanonical                  = "canonical"
	RunCompatLegacyFailed               = "legacy_failed"
	RunCompatLegacyFailedShadowed       = "legacy_failed_shadowed"
	RunCompatMissingStateAssumedStopped = "missing_state_assumed_stopped"
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
		start, terminal, failed, _, ok := classifyRunEvent(e)
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
			run.Failed = failed
		} else if run.TerminalAt.IsZero() {
			run.TerminalAt = e.Timestamp
		}
	}

	out := make([]RunLifecycle, 0, len(order))
	for _, key := range order {
		run := *runs[key]
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

func classifyRunEvent(e Event) (start, terminal, failed, missingState, ok bool) {
	switch e.Type {
	case EventAgentStarted:
		return true, false, false, false, true
	case EventAgentFailed:
		return false, true, true, false, true
	case EventAgentCompleted:
		state, hasState := e.Data["state"].(string)
		if !hasState || state == "" {
			return false, true, false, true, true
		}
		return false, true, state != "stopped", false, true
	default:
		return false, false, false, false, false
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
		state, _ := run.TerminalEvent.Data["state"].(string)
		if state == "" {
			return RunCompatMissingStateAssumedStopped
		}
		return RunCompatCanonical
	default:
		return RunCompatCanonical
	}
}
