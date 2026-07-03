package agent

import (
	"context"
	"errors"
	"time"

	"github.com/Automaat/sybra/internal/events"
)

// attemptEventsFrom slices the events produced since prevLen out of all,
// clamping prevLen so a buffer that shrank between reads (should not happen,
// but is cheap to guard) never panics. Shared by the StreamEvent (headless)
// and ConvoEvent (conversational) attempt-scoping call sites.
func attemptEventsFrom[T any](all []T, prevLen int) []T {
	if prevLen > len(all) {
		prevLen = len(all)
	}
	return all[prevLen:]
}

// runRetryLoop drives the attempt/backoff/retry state machine shared by
// runHeadless and runConversational: it waits out headlessRetryBackoffs
// between attempts, invokes attempt once per iteration, and reacts to its
// (retry, err) result the same way regardless of provider mode.
//
// Returns true when the caller must return immediately without finalizing —
// either the app is shutting down on a detached, not-intentionally-stopped
// agent (leave it for the next reattach), the attempt left a detached
// subprocess running across shutdown (errSurviveShutdown), or the attempt
// returned a fatal error (already handled via m.handleError). Returns false
// when the loop broke out normally (or exhausted retries) and the caller
// should proceed to its own finalize step.
func (m *Manager) runRetryLoop(ctx context.Context, a *Agent, logTag string, attempt func(n int) (retry bool, err error)) (earlyReturn bool) {
	for n := range len(headlessRetryBackoffs) + 1 {
		if n > 0 {
			wait := headlessRetryBackoffs[n-1]
			m.logger.Info("agent."+logTag+".retry", "id", a.ID, "attempt", n, "backoff", wait)
			select {
			case <-ctx.Done():
				if a.isDetached() && !a.WasStopped() {
					return true
				}
				return false
			case <-time.After(wait):
			}
		}

		retry, fatalErr := attempt(n)
		if errors.Is(fatalErr, errSurviveShutdown) {
			return true
		}
		if fatalErr != nil {
			m.handleError(ctx, a, fatalErr)
			return true
		}
		if !retry {
			break
		}
		if n == len(headlessRetryBackoffs) {
			m.logger.Error("agent."+logTag+".retry.exhausted", "id", a.ID, "attempts", len(headlessRetryBackoffs))
		}
	}
	return false
}

// finalizeRun marks a completed run stopped, emits the terminal state/events,
// and releases the agent. Shared tail of runHeadless and runConversational
// once runRetryLoop has broken out normally.
func (m *Manager) finalizeRun(ctx context.Context, a *Agent, doneLogEvent string) {
	a.SetState(StateStopped)
	m.logger.Info(doneLogEvent, "id", a.ID, "cost", a.GetCostUSD())
	m.emit(events.AgentState(a.ID), a)
	m.fireComplete(ctx, a, a.GetExitErr() == nil)
	m.markAgentDone(a)
}
