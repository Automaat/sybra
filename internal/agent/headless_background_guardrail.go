package agent

// backgroundTaskGuardrail is appended to every one-shot, code-authoring run's
// prompt (see RunConfig.SeedWorkingMemory / Role.AuthorsCode): the CLI
// process exits as soon as the agent emits its final response, and it tears
// down any live background work at that point — a backgrounded bash task
// (`run_in_background`) or a subagent dispatched in the background (e.g. the
// Agent/Task tool) alike — Sybra's own process supervision never gets a
// chance to wait longer, because the child has already decided to exit. A
// command killed mid-write leaves the worktree silently corrupted rather than
// failing loudly: task 3aeabb65 traced a deterministic verify_checks failure
// to a `npm ci` backgrounded and then interrupted by end-of-turn, which left
// `node_modules` with hundreds of empty package directories; task e150a89b's
// lost_agent incident (see monitor investigation 73474d71) showed the same
// shape on an implementation run whose final turn deferred `make check` to
// the background and ended anyway. Task f884134e showed the subagent variant:
// an implementation run delegated the work to a background subagent, said it
// would wait for it, and ended anyway — the subagent was killed before it
// produced any edits, so the run finished with zero commits. The only
// reliable fix is telling the agent never to end its turn with background
// work — bash or subagent — still outstanding.
const backgroundTaskGuardrail = "\n\n---\n\n" +
	"This turn is one-shot: the underlying process exits as soon as you " +
	"finish your final response, which immediately kills any background work " +
	"you left running — a command backgrounded via `run_in_background` (e.g. a " +
	"Bash call) or a subagent dispatched in the background (e.g. the Agent/Task " +
	"tool). A command killed mid-write can leave the worktree silently " +
	"corrupted — for example a killed `npm ci` leaves an empty, broken " +
	"`node_modules` that fails every later build deterministically, not " +
	"flakily. A subagent killed mid-delegation leaves the implementation " +
	"undone — do not delegate the required code changes to a background " +
	"subagent and end your turn saying you will wait for it; that subagent is " +
	"killed with you and nothing gets implemented, committed, or pushed. Never " +
	"end your turn while background work is still running: run slow commands " +
	"(`npm ci`, `mise run verify`, test suites, etc.) and any subagent you " +
	"delegate to in the foreground, or poll until it finishes before your " +
	"final response."

// withBackgroundTaskGuardrail appends backgroundTaskGuardrail to prompt for
// one-shot code-authoring headless runs — the shape where a backgrounded
// command can be silently killed and corrupt the worktree.
func withBackgroundTaskGuardrail(prompt string, cfg RunConfig) string {
	if !cfg.SeedWorkingMemory {
		return prompt
	}
	if cfg.Mode != "headless" {
		return prompt
	}
	return prompt + backgroundTaskGuardrail
}
