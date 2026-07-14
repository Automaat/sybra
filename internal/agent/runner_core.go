package agent

import (
	"context"
	"errors"
	"os/exec"
	"time"

	"github.com/Automaat/sybra/internal/events"
)

// newProviderCmd is the sole constructor for a provider CLI subprocess. Every
// exec.CommandContext spawn of a provider binary (claude/codex/copilot/opencode, in
// any of headless-pipe, headless-survive, persistent-convo, convo-survive,
// or per-turn-convo mode) must go through here so a single seam can wrap the
// invocation for OS-level sandboxing (see wrapInvocation in
// procsandbox_darwin.go/procsandbox_other.go) — a new spawn site cannot
// obtain an unsandboxed provider process by construction.
//
// detached selects the shutdown-behavior seam: false configures graceful
// SIGTERM-then-SIGKILL shutdown on ctx cancellation (configureGracefulShutdown)
// for a live, pipe-backed spawn; true configures a detached process group
// (configureDetached) for a spawn that must survive a cancelled parent ctx —
// in that case the child is run under context.Background() instead of ctx, so
// a shutdown cancel never reaches it. cfg carries the sandbox spec computed by
// prepareRunConfig; a nil cfg spawns unwrapped (e.g. in tests that construct a
// command directly).
func newProviderCmd(ctx context.Context, cfg *RunConfig, detached bool, name string, args ...string) *exec.Cmd {
	wrappedName, wrappedArgs := wrapInvocation(name, args, cfg)
	var cmd *exec.Cmd
	if detached {
		// no Context: a cancelled ctx must not kill a detached child
		cmd = exec.CommandContext(context.Background(), wrappedName, wrappedArgs...) //nolint:contextcheck // detached child must survive a cancelled parent ctx
		configureDetached(cmd)
	} else {
		cmd = exec.CommandContext(ctx, wrappedName, wrappedArgs...)
		configureGracefulShutdown(cmd)
	}
	return cmd
}

// sandboxSpec carries the resolved OS-level process-sandbox posture and
// canonicalized allowed write roots for one run, computed once by
// Manager.injectProcessSandbox (manager_run.go) and consumed by
// wrapInvocation (procsandbox_darwin.go / procsandbox_other.go) at each
// provider spawn site. The zero value ("") is equivalent to "off": unwrapped.
type sandboxSpec struct {
	// mode is "off" or "enforce" here — never "report": report-mode specs are
	// validated and logged by injectProcessSandbox but always stored as "off"
	// so a profile/SBPL defect can only affect an explicit enforce posture,
	// never the default rollout posture.
	mode         string
	worktree     string
	gitMetadata  []string
	sandboxHome  string
	tmp          string
	sharedCache  string
	profilePath  string
	gitAdminDir  string
	gitCommonDir string
	gitWorktrees string

	claudeState   string
	codexState    string
	copilotState  string
	opencodeState string
	toolCache     string
}

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
	m.markAgentDone(ctx, a)
}
