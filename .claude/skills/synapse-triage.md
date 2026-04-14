---
name: synapse-triage
description: Triage Synapse tasks — delegate to the Go classifier which rewrites the title, assigns tags/mode/status, and matches a project in one atomic update. Use when asked to triage, categorize, or prioritize tasks.
allowed-tools: Bash
user-invocable: true
---

# Synapse Task Triage

Classify pending tasks via the Go classifier. Go owns routing rules, tag validation, project auto-match, and atomic multi-field updates. The LLM only produces the structured verdict.

## Process

1. List pending tasks:

   ```bash
   synapse-cli --json list --status new
   ```

2. For each task, run the classifier:

   ```bash
   synapse-cli --json triage classify <id>
   ```

   This makes a single `claude -p` call that:
   - Rewrites the title into a clean imperative conventional-commit form (always, even if the input already looked fine)
   - Preserves the original title in the body
   - Assigns tags from the controlled vocabulary (backend, frontend, infra, docs, ci, auth, db, test + size + type)
   - Picks size (small|medium|large), type (bug|feature|refactor|review|chore|docs), and mode (headless|interactive)
   - Auto-matches a registered project if a github.com URL is in the title or body
   - Applies routing rules (medium/large features → planning; everything else → todo)
   - Forces `interactive` mode for `work` projects unless it's a PR review
   - Writes a `triage.classified` audit event

3. Batch mode for larger queues:

   ```bash
   synapse-cli --json triage classify --all
   ```

## Constraints

- Do NOT call `synapse-cli update` directly during triage — the Go classifier owns every field change. Manual updates will race the classifier and break audit trails.
- Do NOT explore the codebase or read source files — the classifier sees only `{title, body, registered projects}`. Codebase exploration belongs in planning/implementation.
- If `classify` returns an error, flag the task with `synapse-cli update <id> --status human-required --status-reason "triage failed"` and move on.
- Ignore tasks with `role` field set (triage, plan, eval, pr-fix) — those are system agents, not implementation work.
