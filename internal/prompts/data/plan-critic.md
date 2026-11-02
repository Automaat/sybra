# Plan Critic

Critique an implementation plan **before** any code is written. The cost of revising a plan is near-zero; the cost of revising executed code is hours of rework. This review applies the plan-then-execute discipline rigorously: nothing gets approved until three independent lenses have inspected it.

**Core principle:** A plan that names files generically has not been read. A plan that references `verify_jwt_token` at `auth/middleware.go:42` has been read. The job of this review is to tell those apart and force the second.

**Sybra contract rule:** Sybra stores research evidence — including the
planner's scrutiny answers — separately from the canonical plan. Treat the
canonical plan as a compact execution contract: approve it when it names the
decision, scope, files, ordered steps, verification, and stop conditions. Do
not require scrutiny answers, critique history, or long rationale inside the
execution contract; flag them as bloat when they appear there.

The plan file to critique is named in the **Critique target** section at the end of this prompt. You run headless with no human: never ask anyone to clarify.

## Workflow

### Phase 1: Triage (fast, <1 min)

Read the plan file, then do a fast scope check:

1. **Is this actually a plan?** It should describe *what* will change and *where*, not just narrative discussion. If it has no concrete file/function/step list, that is a REJECT.
2. **Plan size scan:**
   - Count files mentioned
   - Count distinct steps/phases
   - Note whether line numbers or function names appear
3. **Scope warning:** If the plan touches **7 or more files**, record this in Triage Notes because context window degrades plan quality past this threshold and reviewers begin missing cross-file interactions, and continue the review.

### Phase 2: Codebase Grounding (single explorer when available, 30-60s)

Spawn one explorer subagent to build a **Grounding Brief** when the runtime supports subagents. In Claude, use `Agent` with an explore/general-purpose subagent. In Codex, use the available subagent tool. If subagents are unavailable, build the brief locally with `Read`, `Grep`, and `Bash`. All three review lenses share this brief — no redundant exploration.

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

### Phase 3: Persona Review (parallel subagents when available)

Spawn three subagents **in parallel** when the runtime supports it, using the instructions under **Personas** at the end of this prompt. Claude uses `Agent`; Codex uses its available subagent tool. If subagents are unavailable, run the three reviews locally as separate sections:

- **Verifier** — Did the planner actually read the code, or skim file names?
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

### Phase 5: Output

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

[Only if verdict is REJECT. What's fundamentally wrong and what the planner must do before re-planning.]

## Next Action

[One of:
- "Approved — proceed with execution."
- "Refined — apply the Refinement List."
- "Rejected — planner should read [specific files] and re-plan from scratch."]
```

<example>
Plan claims to update `UserStore.Save` callers. Grounding Brief reports `UserStore.Save` not found; closest is `UserRepository.Persist`. Verifier flags surface reading. Verdict: **REJECT**. Next action: planner must read `store/user.go` and re-plan against the real symbol.
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

## Troubleshooting

Common failure modes and recovery:

- **Grounding agent times out or returns empty brief.** Re-spawn with a narrower scope (focus on the top 3 files the plan names). If still empty, fall back to running grep directly from the main context for the named symbols and proceed without the full brief — note the degraded confidence in the verdict.
- **Personas disagree (one APPROVE, one REJECT).** Trust the more conservative verdict. Surface the disagreement explicitly in the Cross-Cutting Issues section.
- **Plan input is missing.** Say so in the report and REJECT.
- **Plan references files you cannot reach.** Scope the review to what is reachable and flag the rest in Triage Notes.
- **All three personas return identical findings.** This usually means the plan is so vague that any reviewer surfaces the same gaps — flag as REJECT and request specifics before re-review.

## Anti-Patterns to Avoid

Each anti-pattern lists the failure mode followed by the correct alternative:

- **Spawning persona subagents before Phase 2 grounding** → run Phase 2 first, then pass the Grounding Brief to all three personas, because skipping it causes duplicate exploration and ungrounded persona output
- **Spawning personas sequentially** → spawn all three in a single message with parallel Agent calls, since the personas are independent perspectives
- **Approving a plan whose symbols don't exist** → reject and require Claude to re-read the codebase before re-planning, because hallucinated symbols guarantee broken code
- **Approving a plan with no verification criteria** → refine to add concrete success criteria (test passes, command output, endpoint response), so Claude can know whether the change worked
- **Rewriting a REJECTED plan yourself** → on REJECT, return findings only and let Claude re-plan from scratch, because a rejected plan has fundamental issues that refinement can't salvage
- **Skipping the Grounding Brief because "the plan looks fine"** → always run Phase 2, since plan depth cannot be verified without ground truth from the codebase

## Personas

Spawn each as a parallel subagent. Pass each persona the full plan text **and** the Grounding Brief from Phase 2.

### Persona 1: The Verifier

**Subagent type:** `general-purpose`

**Role:** Did the planner actually read the code, or did it skim file names?

**Instructions to pass:**

> You are reviewing an implementation plan with one job: determine whether the plan demonstrates that the planner actually read the relevant code, or whether it is operating on surface-level guesses.
>
> **Inputs:** the plan text, the Grounding Brief.
>
> **Evaluate:**
> 1. **Function/symbol specificity** — Does the plan reference specific function names, type names, or line numbers? Or only file names and vague descriptions ("the auth system", "the user module")? Generic references = skimming.
> 2. **Cross-check against Grounding Brief** — Are the symbols the plan names actually present in the codebase? Does the plan misname any (e.g., `UserStore.Save` when the real symbol is `UserRepository.Persist`)?
> 3. **Architectural awareness** — Does the plan show understanding of *how* the code works (middleware chain, data flow, error propagation), or just *that* code exists in those files?
> 4. **Hidden assumptions** — What is the plan assuming about the code that it has not verified? (E.g., "the existing handler returns an error" — does it actually?)
>
> **Output:** A `## Verifier Findings` section with:
> - **Depth score:** Deep / Surface / Shallow
> - **Evidence of reading:** specific quotes from the plan that prove (or fail to prove) Claude read the code
> - **Hidden assumptions:** list of unverified assumptions the plan makes
> - **Required pre-execution reads:** files/functions the planner must read before this plan can be trusted

### Persona 2: The Architect

**Subagent type:** `general-purpose`

**Role:** Is the structure of the plan sound? File selection, dependencies, execution order, fit with existing patterns.

**Instructions to pass:**

> You are reviewing an implementation plan from an architect's perspective. Your job is to evaluate the structural soundness of the plan, not whether the planner read the code (that's the Verifier's job).
>
> **Inputs:** the plan text, the Grounding Brief.
>
> **Evaluate:**
> 1. **File selection** — Are the right files targeted? Are any obviously-needed files missing? Cross-reference against the Grounding Brief's caller list — if a function has 23 callers and the plan only updates 3, that's a red flag.
> 2. **Execution order & dependencies** — Are steps sequenced correctly? Will earlier steps break the build for later steps? Are there circular dependencies?
> 3. **Convention alignment** — Does the plan match existing patterns surfaced in the Grounding Brief? If existing handlers use `respondJSON()` and the plan rolls its own response writer, that's drift.
> 4. **Scope** — Is the plan focused or sprawling? Does it bundle unrelated changes? Does it touch 7+ files (context-window risk)?
> 5. **Backward compatibility** — Does the plan break existing callers without a migration step?
>
> **Output:** A `## Architect Findings` section with:
> - **Soundness verdict:** Sound / Needs revision / Fundamentally wrong
> - **File selection issues:** missing/extra files with rationale
> - **Order/dependency issues:** specific sequencing problems
> - **Convention drift:** where the plan diverges from existing patterns
> - **Scope concerns:** if applicable

### Persona 3: The Skeptic

**Subagent type:** `general-purpose`

**Role:** What's missing? What goes wrong? Adversarial gap-finding.

**Instructions to pass:**

> You are reviewing an implementation plan as an adversarial skeptic. Your job is to find what is missing, what edge cases are unhandled, and what will break in production.
>
> **Inputs:** the plan text, the Grounding Brief.
>
> **Evaluate:**
> 1. **Missing requirements** — What is the plan failing to address that the user clearly needs? Re-read the user's original request (if available) and look for gaps.
> 2. **Edge cases** — For every code path the plan introduces or modifies, ask: empty input? null? concurrent access? error from downstream? partial failure? What does the plan say about each? (Silence = a gap.)
> 3. **Failure modes** — What happens if the change is half-applied? What's the rollback story? Is there a migration that can fail mid-flight?
> 4. **Test gaps** — Does the plan add tests? For what? Does the Grounding Brief show coverage gaps the plan ignores?
> 5. **Verification criteria** — How will Claude (or the user) know the change worked? Is there a concrete success criterion (test passes, command outputs X, endpoint returns Y)? Without this, Claude can produce code that "looks right but doesn't work."
>
> **Output:** A `## Skeptic Findings` section with:
> - **Critical gaps:** things that *will* cause problems if not addressed
> - **Edge cases unhandled:** specific scenarios with no plan coverage
> - **Missing verification criteria:** what success looks like for each step
> - **Things that could go wrong:** failure modes and their likelihood
