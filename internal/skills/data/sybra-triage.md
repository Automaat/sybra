---
name: sybra-triage
description: Triage Sybra tasks — delegate to the Go classifier which rewrites the title, assigns tags/mode/status, and matches a project in one atomic update. Use when asked to triage, categorize, or prioritize tasks.
allowed-tools: Bash
user-invocable: true
---

# Sybra Task Triage

Classify pending tasks via the Go classifier. Go owns routing rules, tag validation, project auto-match, and atomic multi-field updates. The LLM only produces the structured verdict.

## Process

1. List pending tasks:

   ```bash
   sybra-cli --json list --status new
   ```

2. For each task, run the classifier:

   ```bash
   sybra-cli --json triage classify <id>
   ```

   This makes a single `claude -p` call that:
   - Rewrites the title into a clean imperative conventional-commit form (always, even if the input already looked fine)
   - Preserves the original title in the body
   - Assigns tags from the controlled vocabulary (backend, frontend, infra, docs, ci, auth, db, test + size + type), plus `noplan` when the task is small and trivially mechanical (dep bumps, CI fixes, typo/docs) so it skips planning
   - Picks size (small|medium|large), type (bug|feature|refactor|review|chore|docs), and mode (headless|interactive)
   - Auto-matches a registered project if a github.com URL is in the title or body
   - Applies routing rules (work non-reviews → planning; medium/large features → planning; everything else → todo)
   - The plan workflow honors two escape-hatch tags on a task — `noplan` skips planning entirely (triage → implement, and for work tasks also skips the human plan-review gate), `nocritic` keeps planning but skips the plan critique. The classifier now **assigns `noplan` itself** for trivially mechanical small tasks, bounded by a deterministic floor (emitted only when size is `small` and type is not `feature`; otherwise stripped in `ValidateVerdict`). It also **preserves** either tag when already set, so a human/orchestrator can still force the opt-out on any task before triage and it survives unchanged.
   - Forces `interactive` mode for `work` projects unless it's a PR review
   - Writes a `triage.classified` audit event

3. Batch mode for larger queues:

   ```bash
   sybra-cli --json triage classify --all
   ```

## Constraints

- Do NOT call `sybra-cli update` directly during triage — the Go classifier owns every field change. Manual updates will race the classifier and break audit trails.
- Do NOT explore the codebase or read source files — the classifier sees only `{title, body, registered projects}`. Codebase exploration belongs in planning/implementation.
- If `classify` returns an error, flag the task with `sybra-cli update <id> --status human-required --status-reason "triage failed"` and move on.
- Ignore tasks with `role` field set (triage, plan, eval, pr-fix) — those are system agents, not implementation work.
