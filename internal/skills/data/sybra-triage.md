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

   This makes a single provider-fallback classifier call that:
   - Rewrites the title into a clean imperative conventional-commit form (always, even if the input already looked fine)
   - Preserves the original title in the body
   - Assigns tags from the controlled vocabulary (backend, frontend, infra, docs, ci, auth, db, test + size + type), plus `noplan` and/or `trivial` when the task is small and trivially mechanical (dep bumps, CI fixes, typo/docs) so it skips planning (`noplan`) and, for the same class of task, also review + testing (`trivial`)
   - Picks size (small|medium|large), type (bug|feature|refactor|review|chore|docs), and mode (headless|interactive)
   - Auto-matches a registered project if a github.com URL is in the title or body
   - Applies routing rules (work non-reviews → planning; medium/large features → planning; everything else → todo)
   - The plan/review/test workflows honor four escape-hatch tags on a task — `noplan` skips planning entirely (triage → implement, and for work tasks also skips the human plan-review gate), `nocritic` keeps planning but skips the plan critique, `trivial` skips both agentic code review and adversarial manual testing (straight to opening the PR after implementation), and `skip-testing` skips only adversarial manual testing after review has run. The classifier now **assigns `noplan` and/or `trivial` itself** for trivially mechanical small tasks, bounded by a deterministic floor (emitted only when size is `small` and type is not `feature`; `trivial` is additionally never emitted for type `bug` since it skips both verification gates; otherwise stripped in `ValidateVerdict`). `skip-testing` remains human/orchestrator-set only. Triage **preserves** any of these tags when already set, so a human/orchestrator can still force the opt-out on any task before triage and it survives unchanged.
   - Forces `interactive` mode for `work` projects unless it's a PR review
   - Writes a `triage.classified` audit event

3. Batch mode for larger queues:

   ```bash
   sybra-cli --json triage classify --all
   ```

## Constraints

- Do NOT call `sybra-cli update` directly during triage — the Go classifier owns every field change. Manual updates will race the classifier and break audit trails.
- Do NOT explore the codebase or read source files — the classifier sees only `{title, body, registered projects}`. Codebase exploration belongs in planning/implementation.
- If `classify` returns an error, do **not** call `sybra-cli update` and do **not** move the task to `human-required`. The CLI preserves the existing title/tags and records a retryable `status_reason`; leave the non-zero result visible so the workflow can retry or block as infrastructure failure.
- Ignore tasks with `role` field set (triage, plan, eval, pr-fix) — those are system agents, not implementation work.
