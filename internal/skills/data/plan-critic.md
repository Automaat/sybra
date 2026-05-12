---
name: plan-critic
description: Critique an implementation plan before execution. Works with Claude or Codex; uses subagents when available and falls back to local review.
argument-hint: "[plan file path | pasted plan text | --from-conversation]"
user-invocable: true
allowed-tools: Read, Grep, Bash, Write, Edit, Agent
---

# Plan Critic

Review an implementation plan before code is written. Reject plans that are vague, ungrounded, risky, or missing verification.

## Inputs

- File path: read the markdown file.
- Inline text: review the pasted plan.
- `--from-conversation`: review the latest plan in the conversation.

If the input is ambiguous, ask for the plan source.

## Process

1. Resolve the plan input.
2. Fast triage:
   - Is it actually a plan?
   - Does it name concrete files, symbols, commands, and ordered steps?
   - Is the scope small enough to execute safely?
3. Ground the plan in the repo:
   - Verify named files exist.
   - Verify named symbols exist.
   - Find relevant callers and established local patterns.
   - Check whether verification commands match the project.
4. Review from three lenses:
   - Verifier: did the planner read the code?
   - Architect: is the sequence and boundary choice sound?
   - Skeptic: what breaks, what is missing, what is untested?
5. Use subagents in parallel when the runtime supports them. If not, perform the three lenses locally and keep the sections separate.
6. Decide:
   - `APPROVE`: plan is executable as-is.
   - `REFINE`: plan is sound but needs specific edits.
   - `REJECT`: plan is wrong, vague, or ungrounded.
7. For `REFINE`, if reviewing a file and edits are allowed, update the plan file directly. Otherwise include a refined plan section.
8. Save the review when possible:

```bash
mkdir -p ~/.claude/plan-reviews
```

Use a timestamped markdown file. If that path is unavailable, print the review only.

## Output Format

```markdown
# Plan Review: <APPROVE|REFINE|REJECT>

## Verdict

<one concise paragraph>

## Findings

- [severity] <file/symbol/step>: <issue and consequence>

## Required Changes

- <change or "None">

## Refined Plan

<only when verdict is REFINE and the source plan was not edited directly>

## Verification

- <command> -> <expected result>
```

## Rules

- Findings first.
- Cite files and symbols precisely.
- Do not approve plans with hallucinated files or symbols.
- Do not implement code.
- Do not change task status.
- Do not use placeholders.
