package agent

// backgroundTaskGuardrail is appended to every headless, code-authoring run's
// prompt (see RunConfig.SeedWorkingMemory / Role.AuthorsCode). Headless mode
// is one-shot: the CLI process exits as soon as the agent emits its final
// response, and it tears down any live background bash task
// (`run_in_background`) at that point — Sybra's own process supervision
// never gets a chance to wait longer, because the child has already decided
// to exit. A command killed mid-write leaves the worktree silently
// corrupted rather than failing loudly: task 3aeabb65 traced a deterministic
// verify_checks failure to a `npm ci` backgrounded and then interrupted by
// end-of-turn, which left `node_modules` with hundreds of empty package
// directories. The only reliable fix is telling the agent never to end its
// turn with a background task still running.
const backgroundTaskGuardrail = "\n\n---\n\n" +
	"Headless turns are one-shot: the underlying process exits as soon as you " +
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
// headless code-authoring runs — the only shape where a backgrounded command
// can be silently killed and corrupt the worktree. Interactive/conversational
// sessions persist across turns, so a background task started there survives
// past any single turn ending; this is a headless-only failure mode.
func withBackgroundTaskGuardrail(prompt string, cfg RunConfig) string {
	if cfg.Mode != "headless" || !cfg.SeedWorkingMemory {
		return prompt
	}
	return prompt + backgroundTaskGuardrail
}
