package agent

// backgroundTaskGuardrail is appended to every one-shot, code-authoring run's
// prompt (see RunConfig.SeedWorkingMemory / Role.AuthorsCode): headless, and
// interactive dispatched with RunConfig.OneShot (a single detached
// conversational turn — see OneShot's doc comment). Both are one-shot: the
// CLI process exits as soon as the agent emits its final response, and it
// tears down any live background bash task (`run_in_background`) at that
// point — Sybra's own process supervision never gets a chance to wait
// longer, because the child has already decided to exit. A command killed
// mid-write leaves the worktree silently corrupted rather than failing
// loudly: task 3aeabb65 traced a deterministic verify_checks failure to a
// `npm ci` backgrounded and then interrupted by end-of-turn, which left
// `node_modules` with hundreds of empty package directories; task e150a89b's
// lost_agent incident (see monitor investigation 73474d71) showed the same
// shape on an interactive OneShot implementation run, whose final turn
// deferred `make check` to the background and ended anyway. The only
// reliable fix is telling the agent never to end its turn with a background
// task still running.
const backgroundTaskGuardrail = "\n\n---\n\n" +
	"This turn is one-shot: the underlying process exits as soon as you " +
	"finish your final response, which immediately kills any command you left " +
	"running in the background (e.g. a `run_in_background` Bash call). A " +
	"command killed mid-write can leave the worktree silently corrupted — for " +
	"example a killed `npm ci` leaves an empty, broken `node_modules` that " +
	"fails every later build deterministically, not flakily. Never end your " +
	"turn while a background task is still running: run slow commands " +
	"(`npm ci`, `mise run verify`, test suites, etc.) in the foreground and " +
	"wait for them to exit, or poll the background task until it finishes " +
	"before your final response."

// withBackgroundTaskGuardrail appends backgroundTaskGuardrail to prompt for
// one-shot code-authoring runs — headless, and interactive dispatched with
// OneShot — the shapes where a backgrounded command can be silently killed
// and corrupt the worktree. A regular (non-OneShot) interactive/conversational
// session persists across turns, so a background task started there survives
// past any single turn ending; that shape does not need the guardrail.
func withBackgroundTaskGuardrail(prompt string, cfg RunConfig) string {
	if !cfg.SeedWorkingMemory {
		return prompt
	}
	oneShotTurn := cfg.Mode == "headless" || (cfg.Mode == "interactive" && cfg.OneShot)
	if !oneShotTurn {
		return prompt
	}
	return prompt + backgroundTaskGuardrail
}
