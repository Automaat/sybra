---
name: plan-critic
description: Critique a Claude implementation plan before approving execution. Spawns parallel persona subagents (Skeptic, Architect, Verifier) to evaluate the plan against the codebase, auto-applies refinements on REFINE verdict, and saves every review to ~/.claude/plan-reviews/ for future reference. Use when a plan has been generated (Plan Mode output, ExitPlanMode, pasted plan text, or plan file path) and needs review before code is written. Triggers on "critique this plan", "review this plan", "is this plan good", "poke holes in this plan", "should I approve this plan", or sharing a plan for evaluation.
argument-hint: "[plan file path | paste plan inline | --from-conversation]"
user-invocable: true
allowed-tools:
  - Agent
  - Read
  - Grep
  - Bash
  - Edit
  - Write
  - AskUserQuestion
---

# Plan Critic

Critique a Claude implementation plan **before** any code is written. The cost of revising a plan is near-zero; the cost of revising executed code is hours of rework. This skill applies the plan-then-execute discipline rigorously: nothing gets approved until three independent personas have inspected it.

**Core principle:** A plan that names files generically has not been read. A plan that references `verify_jwt_token` at `auth/middleware.go:42` has been read. The job of this skill is to tell those apart and force the second.

## Arguments

| Argument | Type | Description |
| :-- | :-- | :-- |
| `[plan file path]` | positional | Path to a markdown file containing the plan. Read with the Read tool. |
| `[paste plan inline]` | positional | Plan text pasted directly in the user's message. Use as-is. |
| `--from-conversation` | flag | Use the plan most recently produced in the current conversation (e.g., from ExitPlanMode or a planning response). |

If input is ambiguous (multiple candidates, unclear which plan), use AskUserQuestion to disambiguate before proceeding. Present these options:

- **File path** — the user types a path to a plan markdown file
- **Paste inline** — the user pastes the plan text in the next message
- **Use `--from-conversation`** — adopt the most recent plan produced in this conversation

## Workflow

### Phase 1: Triage (fast, <1 min)

Resolve the plan input first: read the file if a path was given, use the inline text if pasted, or fetch the most recent plan from the conversation if `--from-conversation` was passed. Then do a fast scope check:

1. **Is this actually a plan?** It should describe *what* will change and *where*, not just narrative discussion. If it has no concrete file/function/step list, ask the user to convert it into a plan first.
2. **Plan size scan:**
   - Count files mentioned
   - Count distinct steps/phases
   - Note whether line numbers or function names appear
3. **Trivial-change escape hatch:** If the plan describes a change that fits in one sentence (typo, single-line fix, rename in one file), tell the user planning overhead isn't warranted — recommend skipping plan review and just executing. Do not spawn subagents for trivial plans.
4. **Scope warning:** If the plan touches **7 or more files**, surface this immediately as a concern because context window degrades plan quality past this threshold and reviewers begin missing cross-file interactions. Recommend the user split it into sub-plans before review continues, so each sub-plan stays small enough to verify deeply, or proceed with explicit acknowledgment.

If triage finds fundamental issues (not a plan, trivial, oversized), report and stop. Do not spawn subagents.

<example>
Input: User pastes a one-line plan: "Fix typo in README.md line 42."
Triage result: Trivial single-line change. Report "Planning overhead isn't warranted — execute directly." Skip Phase 2-5.
</example>

<example>
Input: A plan touching 14 files across 4 packages with no symbol-level references.
Triage result: Scope warning (7+ files) AND surface-level reading risk. Recommend splitting into sub-plans before review continues.
</example>

### Phase 2: Codebase Grounding (single Explore agent, 30-60s)

Spawn one `Agent` (subagent_type: `Explore`, thoroughness: `medium`) to build a **Grounding Brief**. All three review personas will share this brief — no redundant exploration.

The Grounding agent must:

1. For every file the plan names, verify the file exists. List any missing/misnamed paths.
2. For every function, type, or symbol the plan names, grep for it and report the actual location (`path:line`). Flag symbols the plan names that do not exist in the codebase.
3. Identify the top callers of any function the plan proposes to modify (count + 3-5 caller locations).
4. Find existing patterns/conventions for the kind of change the plan proposes (e.g., if the plan adds a new HTTP handler, find 2-3 existing handlers it should mirror).
5. Find related tests for the modules being changed.

**Grounding Brief output format:**

```markdown
## Grounding Brief

### File Verification
- `path/to/a.go` ✓ exists
- `path/to/b.go` ✗ NOT FOUND (plan references it as "the user service")

### Symbol Verification
- `verify_jwt_token` → `auth/middleware.go:42` ✓
- `UserStore.Save` → not found; closest match is `UserRepository.Persist` at `store/user.go:118`

### Callers (blast radius)
- `parseRequest()` — 23 callers (top: handler/api.go:55, handler/admin.go:88, ...)
- `newHelper()` — 0 callers (proposed but never used elsewhere)

### Existing Patterns
- HTTP handlers in `handler/` use `respondJSON()` helper — plan should match
- Error wrapping via `errors.Wrapf` is the convention — plan uses bare `fmt.Errorf`

### Related Tests
- `handler/api_test.go` covers happy path; no error-path tests
- No tests exist for the package the plan modifies most heavily
```

### Phase 3: Persona Review (parallel subagents)

Read [references/personas.md](references/personas.md) in full before spawning Phase 3 agents — it contains the exact instruction blocks for each persona.

Spawn three `Agent` subagents **in parallel** (single message, multiple Agent tool calls) using the prompts in personas.md. Parallelize because the personas are independent perspectives on the same inputs, so sequential spawning wastes wall time without improving quality:

- **Verifier** — Did Claude actually read the code, or skim file names?
- **Architect** — Is the structure sound? File selection, order, conventions, scope?
- **Skeptic** — What's missing? Edge cases, failure modes, verification criteria?

Each persona receives the full plan text **and** the Grounding Brief from Phase 2.

### Phase 4: Synthesis & Verdict

Recall the Triage scope flags from Phase 1 and the Grounding Brief from Phase 2 before reading persona output, so the verdict stays anchored to verified evidence rather than persona assertions. After all three personas return, synthesize their output rather than concatenate it because raw concatenation hides duplication and obscures cross-cutting signals:

1. **Deduplicate** — If Verifier and Architect both flagged the same missing file, merge into one finding.
2. **Rank by severity** — Blocking issues first, then refinements, then nits.
3. **Cross-cutting insights** — Look for findings that only emerge from seeing all three perspectives. Example: Verifier says Claude didn't read `handler.go`, Architect says the plan ignores 20 callers there, Skeptic says no test covers the new behavior — together, this is a much stronger blocker than any one alone.
4. **Apply the Three-Response Framework** to produce a verdict:
   - **APPROVE** — Strategy is sound, depth is adequate, no critical gaps. Safe to execute.
   - **REFINE** — Plan is fundamentally on the right track but needs specific additions/corrections before execution. Provide a concrete refinement list.
   - **REJECT** — Fundamental issues (wrong approach, missed key files, surface-level reading). Plan needs to be redone from scratch after Claude reads more code.

### Phase 5: Auto-Apply & Persist

After synthesis, act on the verdict before producing output. This is a hard requirement — do not skip it. The goal is zero-friction: the user should never have to manually copy refinements back into their plan, and every review should be queryable later so the same issues don't get flagged twice.

**1. Auto-apply (only on REFINE verdict):**

- If the plan was given as a **file path** → use `Edit` to modify the plan file in place. Apply each refinement directly: add missing steps, correct file/symbol references using the Grounding Brief, insert verification criteria, update caller lists. Preserve the plan's original structure and voice — integrate refinements, don't replace the plan wholesale.
- If the plan was **inline** or **`--from-conversation`** → output the refined plan as a fenced markdown block in the final report under a `## Refined Plan` section. The user can copy it into wherever they manage plans.
- On **APPROVE** → nothing to apply.
- On **REJECT** → nothing to apply (plan needs re-doing from scratch). Do not attempt to fix a rejected plan.

**2. Persist the review (always, every verdict):**

Save the full review to `~/.claude/plan-reviews/`. Create the directory if it does not exist:

```bash
mkdir -p ~/.claude/plan-reviews
```

Filename format: `YYYY-MM-DD-HHMMSS-<slug>.md` where `<slug>` is a 3-5 word kebab-case summary derived from the plan's topic (e.g., `2026-04-10-143022-auth-middleware-rewrite.md`). Use `date +%Y-%m-%d-%H%M%S` for the timestamp.

The saved file must contain:

1. Frontmatter: `verdict`, `plan_source` (file path / inline / from-conversation), `plan_file` (absolute path if applicable), `files_touched_count`, `date`
2. The original plan text (before refinement)
3. The full review report (everything from Phase 4 synthesis)
4. If REFINE: the refined plan as applied

This archive exists so future plan reviews can grep prior findings and the user avoids re-litigating the same issues. Reference it in the Next Action line of the output.

### Phase 6: Output

Produce the final report in this exact structure:

```markdown
# Plan Review

## Verdict: [APPROVE | REFINE | REJECT]

**One-line summary:** [Why this verdict in one sentence]

**Plan stats:** [N files, M steps, depth: Deep/Surface/Shallow]

## Triage Notes

[Any flags from Phase 1 — scope warnings, size concerns, etc. Skip if clean.]

## Grounding Brief

[Full output from Phase 2 — file/symbol verification, callers, patterns, tests]

## Verifier Findings

[Output from Persona 1]

## Architect Findings

[Output from Persona 2]

## Skeptic Findings

[Output from Persona 3]

## Cross-Cutting Issues

[Findings that emerge only from combining all three persona perspectives. Skip if none.]

## Refinement List

[Only if verdict is REFINE. Concrete, ordered list of changes to the plan before approval:]
1. Read `path/to/file.go` lines X-Y to verify assumption about Z
2. Add step to update the 14 callers of `funcName` listed in the Grounding Brief
3. Specify success criterion for step 4 (e.g., "tests in `foo_test.go` pass")
4. ...

## Rejection Rationale

[Only if verdict is REJECT. What's fundamentally wrong and what Claude must do before re-planning.]

## Refined Plan

[Only if verdict is REFINE AND plan was inline / --from-conversation. Full refined plan as a fenced markdown block. Omit this section if the plan was a file — in that case the file was edited in place.]

## Review Archive

Saved to: `~/.claude/plan-reviews/<filename>.md`

## Next Action

[One of:
- "Approved — proceed with execution."
- "Refined — plan file updated in place at `<path>`. Review.": when REFINE + file path
- "Refined — refined plan in the Refined Plan section above. Copy into your plan source.": when REFINE + inline/from-conversation
- "Rejected — Claude should read [specific files] and re-plan from scratch."]
```

<example>
Plan claims to update `UserStore.Save` callers. Grounding Brief reports `UserStore.Save` not found; closest is `UserRepository.Persist`. Verifier flags surface reading. Verdict: **REJECT**. Next action: Claude must read `store/user.go` and re-plan against the real symbol.
</example>

<example>
Plan touches 4 files, names 6 symbols with line numbers, all verified by the Grounding Brief. Architect confirms patterns match existing handlers. Skeptic finds one missing edge case (empty payload). Verdict: **REFINE**. Refinement list: one entry — add empty-payload handling in step 3.
</example>

## Calibration Guidance

These principles prevent common plan-review anti-patterns. Re-read this section before producing the verdict in Phase 4 to keep calibration consistent across reviews:

- **Approve valid approaches even when yours differs.** If the plan's approach is valid and an alternative is also valid, approve, because the Three-Response Framework judges *correctness*, not taste.
- **Reject instead of over-refining.** If the refinement list grows past ~5 items, switch to Reject and request a re-plan, since refining a fundamentally broken plan wastes more tokens than restarting.
- **Block on missing verification criteria.** A plan with no success criteria produces code that "looks right but doesn't work," so treat missing criteria as a refine-or-reject every time.
- **Trust the Grounding Brief over the plan's claims.** When the plan says `X exists` and the Brief says `X not found`, the Brief wins because plans hallucinate and greps don't.
- **Praise specificity.** When a plan references `auth/middleware.go:42` and the Verifier confirms it, call this out so good patterns get reinforced.

## When to Skip Plan Review (limitations and scope boundaries)

This skill is not designed for, and you should avoid using it on:

- One-sentence changes (typo, rename, single-line log) — execute directly instead
- Changes already executed — review the diff with `staff-code-review` instead
- Discussion-only outputs that aren't structured plans — ask the user to formalize the plan first

## Troubleshooting

Common failure modes and recovery:

- **Grounding agent times out or returns empty brief.** Re-spawn with a narrower scope (focus on the top 3 files the plan names). If still empty, fall back to running grep directly from the main context for the named symbols and proceed without the full brief — note the degraded confidence in the verdict.
- **Personas disagree (one APPROVE, one REJECT).** Trust the more conservative verdict. Surface the disagreement explicitly in the Cross-Cutting Issues section and let the user adjudicate.
- **Plan input is ambiguous or missing.** Use AskUserQuestion with concrete options (file path, paste inline, use last-conversation plan). Do not guess.
- **Plan references files in a different repo or path the agent can't access.** Stop and ask the user to either provide the files or scope the review to what is reachable.
- **All three personas return identical findings.** This usually means the plan is so vague that any reviewer surfaces the same gaps — flag as REJECT and request specifics before re-review.

## Anti-Patterns to Avoid

Each anti-pattern lists the failure mode followed by the correct alternative:

- **Spawning persona subagents before Phase 2 grounding** → run Phase 2 first, then pass the Grounding Brief to all three personas, because skipping it causes duplicate exploration and ungrounded persona output
- **Spawning personas sequentially** → spawn all three in a single message with parallel Agent calls, since the personas are independent perspectives
- **Approving a plan whose symbols don't exist** → reject and require Claude to re-read the codebase before re-planning, because hallucinated symbols guarantee broken code
- **Approving a plan with no verification criteria** → refine to add concrete success criteria (test passes, command output, endpoint response), so Claude can know whether the change worked
- **Skipping Phase 5 auto-apply / persist** → always edit the plan file in place on REFINE and always save the review to `~/.claude/plan-reviews/`, because the user explicitly opted into auto-apply to avoid re-litigating the same findings across reviews
- **Rewriting a REJECTED plan yourself** → on REJECT, return findings only and let Claude re-plan from scratch, because a rejected plan has fundamental issues that refinement can't salvage
- **Skipping the Grounding Brief because "the plan looks fine"** → always run Phase 2, since plan depth cannot be verified without ground truth from the codebase
