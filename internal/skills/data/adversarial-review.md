---
name: adversarial-review
description: Fast adversarial code review. Red-teams a diff or PR to find the concrete bug, then refutes its own findings to cut false positives. Works with Claude, Codex, or Copilot — uses subagents when the runtime has them, falls back to sequential local passes. Use for routine changes where a full staff review is overkill.
argument-hint: "<pr-url | diff-file | --local>"
user-invocable: true
allowed-tools: Read, Grep, Glob, Bash, Write, Agent
---

# Adversarial Review

Find the bug. That is the whole job.

This is the default review depth for routine changes. It answers exactly one
question — **is this change correct?** — and answers it hard. It does not
evaluate architecture, conventions, dead code, or taste. When those matter,
the operator reaches for `/staff-code-review` instead; do not reinvent it here.

Two passes, opposed:

1. **Code Adversary** — assumes the change is broken and hunts the concrete failure.
2. **Findings Adversary** — assumes the Code Adversary is wrong and tries to refute it.

The second pass exists because an unrefuted adversary nit-bombs. Its job is to
delete findings, not to add them.

## Arguments

| Argument | Meaning |
| :-- | :-- |
| `<pr-url>` | Review a GitHub PR — `gh pr diff <url>` |
| `<diff-file>` | Review a saved patch file |
| `--local` (or no argument) | Review `git diff origin/main...HEAD` in the current worktree |

## Pass 1 — Resolve the diff

Get the changed files and the patch. Read the **surrounding code**, not just the
hunks — the bug usually lives in the interaction between changed and unchanged
lines. Do not build a research brief and do not spawn an explorer; the adversary
greps for what it needs.

If the diff is empty, say so and stop.

## Runtime capability check

Do this once, before Pass 2.

**If your runtime provides a subagent tool** (Claude Code: `Agent`), run Pass 2
and Pass 3 as two independent subagents. Pass each one the prompt below verbatim
plus the diff. Independence is the point — the Findings Adversary must not
inherit the Code Adversary's reasoning, only its output.

**If your runtime has no subagent tool** (Codex, Copilot, or `Agent` is
unavailable), run both passes sequentially in this context, as separate labelled
sections. Then obey this rule, which exists to partly recover the independence
you just lost:

> In Pass 3 you MUST re-open each cited `file:line` with `Read` and re-derive the
> claim from the source. Do not verify a finding from memory of having written
> it. You are looking for your own mistakes; assume they are there.

Self-critique in one context is measurably weaker than an independent refuter.
Compensate by being harsher, not gentler.

## Pass 2 — Code Adversary

> You are the Code Adversary. You assume **this change has a bug, and your job is
> to find it and prove it.** You are not here to praise the design, restate what
> the code does, or offer style preferences.
>
> **A finding is worth raising only if you can name the input or sequence that
> makes it fail.** "This might be fragile" is worthless. "Passing an empty slice
> here panics at `parse.go:88` because `items[0]` runs before the length check"
> is a finding.
>
> Attack on these axes:
>
> 1. **Correctness** — off-by-one, inverted condition, wrong operator, swapped
>    arguments, wrong default, overflow/truncation, nil dereference, type
>    confusion, swallowed error, error compared to the wrong sentinel.
> 2. **Edge & boundary** — empty / zero / negative / max / single-element input,
>    unicode, huge payloads, duplicate keys, missing keys, partial input. Walk
>    each new branch with the input that breaks it.
> 3. **Concurrency** — data races on shared state, check-then-act races, lock
>    ordering deadlock, goroutine leaks with no cancellation path, lost updates.
> 4. **Failure paths** — what is left half-written when the call on line N fails?
>    Resource leaks on the error return. Partial mutation with no rollback.
>    Non-idempotent retries. Missing or unbounded timeouts. Ignored cancellation.
> 5. **Security** — injection, authz missing or applied after the effect, path
>    traversal, SSRF, unsafe deserialization, secret in code or log, missing
>    validation at the trust boundary, TOCTOU.
> 6. **Data integrity** — destructive migration without backfill, dropped writes,
>    wrong transaction scope, ordering assumptions that do not hold.
> 7. **Hidden assumptions** — implicit ordering, nullability, clock, time zone,
>    locale, environment, stable map iteration, that the remote call succeeds.
> 8. **Tests that do not test** — a new test asserting the wrong thing, asserting
>    nothing, mocking the code under test, or that would still pass with the bug
>    it claims to cover. Name the regression it would miss.
>
> Grep for callers to learn what inputs actually reach this code. Check whether
> this area broke before. Reproduce the failure path end to end in your head
> before writing it down.
>
> **Severity by blast radius, not by taste.** Crash, data loss, security, or a
> silent wrong answer is `blocking:` or `issue:`. A real-but-narrow edge case is
> `issue:` or `suggestion:`. You do not file `nit:` — that is not your job.
>
> **No design preferences.** If the code is correct but you would write it
> differently, say nothing.
>
> **Honesty about reach.** If you genuinely could not break a changed path after
> trying, do not invent a finding. A short hard list beats a padded one.

Emit findings as conventional comments:

```
**{blocking|issue|suggestion|question}:** {message — the failing input/sequence and the observed failure}
*Location:* `{path/to/file}:{line}`
```

## Pass 3 — Findings Adversary

> You attack the **findings**, not the code. Default to skepticism: a finding
> survives at its stated severity only if it withstands a genuine attempt to
> break it. For each one:
>
> 1. **Evidence holds.** Re-open the cited `file:line`. Does the code actually do
>    what the finding claims? Misread or non-reproducing → REMOVE or downgrade to
>    `question:`.
> 2. **Severity is earned.** Does a `blocking:` really break a contract, lose
>    data, or create a security risk — or is it preference dressed as a blocker?
> 3. **Location is real.** Does the file exist, and is the line actually in the
>    diff? A hallucinated or out-of-diff location breaks inline posting.
> 4. **In scope.** Pre-existing problems the change did not introduce are
>    `thought:`, not blockers.
> 5. **Actionable.** Does it name a concrete fix, or is it hand-waving
>    ("improve error handling")? Vague blockers cannot be acted on — reword or
>    downgrade.
> 6. **Not duplicated or contradictory.** Merge duplicates. Reconcile findings
>    that contradict each other rather than shipping both.
>
> **Downgrade over delete where you can** — often the right move is `blocking:` →
> `suggestion:`, which keeps the signal at the right size. But delete outright
> when the evidence does not survive.
>
> If you tried to break a finding and could not, say so — that marks it
> high-confidence. Do not rubber-stamp, and do not launch a fresh bug hunt; Pass
> 2 owns that.

## Output

Surviving findings, strongest first, in the conventional-comment format above.
Then exactly one final line — no prose after it:

```
Review Verdict: CLEAN
```

or

```
Review Verdict: NEEDS_FIXES
```

- **CLEAN** — no surviving finding requires a code change. Style notes,
  simplification ideas, and optional suggestions stay CLEAN.
- **NEEDS_FIXES** — at least one surviving `blocking:` or `issue:` should be
  fixed before this lands.

This line gates whether a fix agent runs next. Get it right.

When the caller asks for the review in a file, that verdict line must be the
**first** line of the file, findings below it. When the caller asks you to submit
to GitHub, post CLEAN as an approve and NEEDS_FIXES as change comments.

## Anti-patterns

- ❌ Filing a finding with no failing input — that is a preference, not a bug
- ❌ Padding a clean review to look thorough — CLEAN is a real, useful verdict
- ❌ Reviewing architecture, naming, or dead code — wrong skill, use `/staff-code-review`
- ❌ Skipping Pass 3 because Pass 2 "seemed confident" — unrefuted adversaries nit-bomb
- ❌ Verifying Pass 2's findings from memory instead of re-reading the cited lines
