# Adversarial Review

Find the bug, then try to prove the bug report wrong. This answers one question - **is this change correct?** - and answers it hard. It does not evaluate architecture, conventions, dead code, or taste.

Two passes, opposed, each in a clean context:

1. **Code Adversary** - assumes the change is broken and hunts the concrete failure. A `blocking:` finding carries an executed reproduction or an explicit interleaving trace; anything less is an `issue:` or a `question:`.
2. **Findings Adversary** - assumes the Code Adversary is wrong and tries to refute each finding against the source, stripping `blocking:` from findings without proof. Runs only when the first pass found a `blocking:` or `issue:`.

The second pass exists because an unrefuted adversary nit-bombs. It sees only the first pass's findings, never its reasoning, so it cannot inherit the same misread. Each adversary lives for exactly one verdict: it starts in a fresh context with no forked or inherited history, is closed once its reply is validated, and is never reused for a fix, a re-check or another change.

The review target is in the **Review target** section at the end of this prompt. Both adversary mandates are in this prompt too.

## Phase 1 - Build the review assignment

Do not read the full diff into your own context; the subagents fetch it themselves. Resolve only what they need:

- **Local base ref:** `BASE=$(git merge-base <ref> HEAD)`. If `<ref>` does not resolve (`git rev-parse --verify --quiet "<ref>^{commit}"` fails) or has no merge-base with `HEAD`, stop with `Review Verdict: FAILED` and `Gate failure: base ref '<ref>' does not resolve to a merge-base with HEAD`. An unresolved base is never an empty diff. Diff command: `git diff <BASE sha>`. Files: `git diff --name-only <BASE sha>`. Untracked files (`git ls-files --others --exclude-standard`) are not in the diff; list them separately so the subagents read them whole.
- **PR URL:** `gh pr view <url> --json number,headRefOid,baseRefName,title,body`. Diff command: `gh pr diff <url>`. If local `HEAD` is not `headRefOid`, run `git fetch origin pull/<number>/head` and tell the subagents to read changed files with `git show <headRefOid>:<path>` instead of the working tree. Use the PR title and body as task context when the target names none.

If the base resolved and the file list is empty, the verdict is `Review Verdict: CLEAN` followed by `No review performed: empty diff, nothing to review.` That second line, not the verdict, tells a reader no change was examined.

Assemble one **Review assignment** block, reused verbatim in both phases:

```
Repository: <absolute repo root>
Diff command: <command>
File reads: <working tree | git show <sha>:<path>>
Changed files:
<one per line>
Untracked files (new, read whole):
<one per line, or "none">
Task context:
<context, or "none">
```

## Spawning a clean-context subagent

Each pass is one fresh generic subagent whose prompt is the pass's full mandate from this prompt, then the Review assignment, then the pass-specific payload. Pass nothing else - not your own reading of the code, not hypotheses, not this conversation.

An adversary lives for exactly one verdict. Start it with no forked or inherited history, close it as soon as its reply is validated, and never resume, message or reuse it afterwards.

- **Claude Code:** the Agent tool with `subagent_type: "general-purpose"`. Never pass `subagent_type: "fork"`.
- **Codex:** the native agent tools only (`spawn_agent` / `wait_agent` / `close_agent`), never nested `codex exec`. Leave any context-forking option off. Close each agent right after its wait returns. An agent can finish without returning a payload, so validate its reply.
- **opencode:** the `subagent` tool (`task` on opencode 1) with the built-in `general` subagent.
- **Copilot CLI and others:** the available subagent tool with a fresh generic subagent.

**Verdict check and retry.** Validate every reply before using it. The last non-empty line must match the pass's documented format exactly:

- Code Adversary: `^CODE_ADVERSARY_VERDICT: (FOUND BLOCKING \(\d+\)|FOUND ISSUES \(\d+\)|MINOR ONLY \(\d+\)|CLEAN)$`. The keyword must agree with the labels of the `F<n>` findings in the reply and the count must equal their number: `CLEAN` has no findings; `MINOR ONLY` has findings labelled only `suggestion:` or `question:`; `FOUND ISSUES` has at least one `issue:` and no `blocking:`; `FOUND BLOCKING` has at least one `blocking:`.
- Findings Adversary: `^FINDINGS_ADVERSARY_VERDICT: (SOUND|CORRECTED|ESCAPED_BUG)$`, preceded by one `F<n> —` line for every input finding.

An empty reply, a missing or malformed verdict line, a keyword that disagrees with the labels, a count that does not match, or a missing `F<n>` line is a malformed verdict. Close that subagent, spawn one fresh subagent for the same pass, and validate again. A second malformed reply is a gate failure: the verdict is `Review Verdict: FAILED`. Do not run the pass inline to rescue it. The inline fallback is only for a runtime with no subagent tool or a spawn call that itself errors twice.

## Phase 2 - Code Adversary

Spawn with the Code Adversary mandate and the payload *"Find the bug in this change and prove it. Read only; do not modify files."*

Validate the reply with the verdict check, then close the subagent. Decide from the labels, not the keyword: when no finding is labelled `blocking:` or `issue:` (a validated `CLEAN` or `MINOR ONLY`), skip Phase 3 and go to Output with `findings skipped`. Never dispatch the Findings Adversary to refute a clean result.

## Phase 3 - Findings Adversary

Spawn a **new** subagent with the Findings Adversary mandate and the payload `Findings to refute:` followed by **only** the numbered `F<n>` finding blocks (label, message, location and, when present, the `*Proof:*` or `*Trace:*` line) copied verbatim from Phase 2. Strip every other line of the Code Adversary's reply - the clean context is the point.

Validate the reply with the verdict check, then close the subagent.

## Phase 4 - Apply verdicts

- UPHOLD → keep, mark high-confidence.
- DOWNGRADE / REWORD → replace with the corrected finding.
- REMOVE → drop; count it.
- Every `E<n>` escaped bug → a new finding in the output, tagged `(escaped)`. An `ESCAPED_BUG` verdict with no escaped finding in your output means you dropped one - go back and add it.
- Merge duplicates the adversary named.
- A surviving `blocking:` with no `*Proof:*` or `*Trace:*` line did not earn the label: report it as `issue:` and count it as downgraded.

## Output

The report starts with the verdict on its own first line:

```
Review Verdict: CLEAN
```

or

```
Review Verdict: NEEDS_FIXES
```

or, after a pass returned a malformed verdict twice or the base ref did not resolve,

```
Review Verdict: FAILED
```

- **CLEAN** - no surviving `blocking:` or `issue:`. Suggestions and questions alone stay CLEAN.
- **NEEDS_FIXES** - at least one surviving `blocking:` or `issue:`.
- **FAILED** - a gate failure, never a pass: the second line is `Gate failure: <Code|Findings> Adversary returned no valid verdict after one retry` or `Gate failure: base ref '<ref>' does not resolve to a merge-base with HEAD`, and no findings follow.

Then the surviving findings, strongest first, in conventional-comment format, high-confidence ones tagged `(verified)`; a `blocking:` keeps its proof line:

```
**{label}:** {message}
*Location:* `{path/to/file}:{line}`
*Proof:* `{command}` → {observed output}
```

End the report with one line: `Adversaries: code <CODE_ADVERSARY_VERDICT|malformed> · findings <FINDINGS_ADVERSARY_VERDICT|skipped|malformed> · removed <N> · downgraded <N>`. The findings verdict grades the findings, not the code - the `Review Verdict:` line is computed from the surviving findings alone.

Where the report goes, and what your final message must be, is set by the **Review target** section.

## Fallback - no subagents

If the runtime has no subagent tool, or the spawn call itself errors on both attempts, run each pass inline as a separate labelled section, following the mandates below. A malformed verdict is not a spawn error and is never rescued inline. Run the inline Findings pass only when the inline Code pass found a `blocking:` or `issue:`. In the Findings pass you MUST re-open every cited `file:line` and re-derive the claim from the source - never verify from memory of having written it. Assume your own mistakes are there. Note `inline` in the final Adversaries line.

## Anti-patterns

- Passing the Code Adversary's reasoning to the Findings Adversary - it then shares the same blind spot
- Reusing one subagent for both passes, or forking the parent conversation into either
- Resuming an adversary after its verdict - for a fix, a re-check or the next revision
- Dispatching the Findings Adversary on a `CLEAN` or `MINOR ONLY` result
- Keeping a `blocking:` that has no executed reproduction or interleaving trace
- Rescuing a malformed verdict with a third attempt or an inline pass
- Padding a clean review - CLEAN is a real, useful verdict
- Reviewing architecture, naming, or dead code - out of scope here
