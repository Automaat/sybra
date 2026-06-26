---
name: sybra-handoff
description: Hand a researched, already-decided task off to Sybra for autonomous implementation. Use after you have explored a problem in an Orca (or any) git worktree, agreed on the approach, and want Sybra to skip its own planning and start implementing immediately — reusing this exact worktree. Triggers on "hand this off to Sybra", "let Sybra implement this", "ship this to Sybra", "handoff to sybra". Works under both Claude Code and Codex.
allowed-tools: Bash
user-invocable: true
argument-hint: "[--stage STAGE | --status STATUS] [--title \"...\"] [--plan-file PATH] [--worktree-dir DIR] [--pr N]"
---

# Sybra Handoff

Hand the **current** worktree's task to Sybra for autonomous implementation. The
human did the research and chose the approach here; Sybra takes the agreed plan
and implements it **without re-planning**, running its agents **in this same
worktree** so the work lands where you were just looking.

This is the Orca -> Sybra handoff point: Orca is the research/planning phase,
Sybra is the autonomous execution phase.

## When to use

Hand off at whatever stage you have reached — Sybra picks up from there:
- You have a **plan** but have not implemented → `--stage implement` (default).
- You have **implemented** locally and want agentic review + PR → `--stage review`
  (aliases: `ready-review`, `agentic-review`).
- You have **implemented and reviewed** locally and want the testing gate →
  `--stage testing`.
- You have **implemented, reviewed, and tested** locally and want Sybra to open
  or update the PR from this worktree → `--stage ready-pr`.
- You already **opened a PR** and want Sybra to review it → `--stage pr --pr N`
  (alias: `in-review --pr N`).

In all cases the approach is decided and you want Sybra to carry it the rest of
the way autonomously. Do **not** use for exploratory work that still needs human
direction — keep that in Orca.

Use `--status STATUS` only when you explicitly want raw board placement with no
workflow dispatch. For agentic handoff, prefer `--stage`; stage aliases are
workflow entry points, not just column names.

## What it does

`sybra-cli handoff` creates a task that skips straight to the requested stage —
no triage, no planning gate — and (for all non-PR stages) reuses **this** git
worktree (`--worktree-dir`, default: cwd) instead of creating a fresh one: no
rebase, no force-push, and Sybra never deletes it.

`sybra-cli handoff --status <status>` creates the task directly in that status
with a `handoff-manual` tag and does **not** start any workflow. This is for
manual board placement only.

Stages:
- **implement** (default): flips to `in-progress`; the implementation agent runs
  now with your plan as context, then review → PR.
- **review**: flips to `ready-review`; Sybra reviews your existing commits in the
  worktree and opens the PR (no implementation step).
- **testing**: flips to `testing`; Sybra runs the adversarial testing gate in the
  adopted worktree and opens the PR if tests pass.
- **ready-pr**: flips to `ready-pr`; Sybra opens or updates the PR from the
  adopted worktree (no implementation, review, or testing step).
- **pr**: for an existing PR (`--pr N`); Sybra reviews the open PR via its
  pr-review lane (no worktree adoption — it checks out the PR head itself).

## Workflow entry context

Pick the stage by the **next workflow Sybra should run**, not only by the visible
column label:

| CLI stage | Aliases | Workflow entry | Use when |
| --- | --- | --- | --- |
| `implement` | `in-progress` | `simple-task-handoff` -> `simple-task-implement` | The approach is decided but code is not written. |
| `review` | `ready-review`, `agentic-review` | `simple-task-handoff-review` -> `simple-task-review` | Code exists in this worktree and needs Sybra's review/test/PR flow. |
| `testing` | `test` | `simple-task-handoff-testing` -> `testing-task` | Code has already had the review you want and only needs Sybra's test/PR flow. |
| `ready-pr` | `open-pr`, `create-pr` | `simple-task-handoff-ready-pr` -> `simple-task-pr` | Code has already been reviewed and tested, and Sybra should open/update the PR from this worktree. |
| `pr` | `in-review`, `pull-request` | `pr-review` | A real PR already exists; pass `--pr N`. |

Important status distinction:
- `ready-review` / `agentic-review` means "start Sybra's review workflow".
- `ready-pr` means "open or update a PR from this adopted worktree".
- `in-review` means "an existing PR is already open and needs PR review"; it
  requires `--pr N` and does not adopt the local worktree.
- `--status in-review` means "place this task in the in-review column without
  starting any review workflow"; use this only when a human or another system is
  handling the next action.

If unsure, inspect `internal/workflow/builtin/*.yaml` and choose the entry point
whose trigger matches the next agentic step. Do not add broad workflow-lane tags
such as `review` unless you intentionally want the existing-PR lane.

## Procedure

1. **Confirm the worktree and branch.** Run from the worktree root:
   ```bash
   pwd && git rev-parse --abbrev-ref HEAD && git remote get-url origin
   ```
   The branch must not be the default branch (Sybra commits + opens a PR on it).
   The origin remote must be a GitHub repo registered as a Sybra project.

2. **Write the approved plan to a file.** Synthesize the decision reached in this
   session into a concrete, file-level plan (name the files and symbols to
   change, the ordered steps, and how to verify each), then write it to a file
   in the worktree — e.g. via a heredoc so this works with shell-only tools:
   ```bash
   cat > ./.sybra-handoff-plan.md <<'PLAN'
   # Plan
   ... your file-level plan here ...
   PLAN
   ```
   A plan that names `verify_jwt_token` at `auth/middleware.go:42` has been
   thought through; one that says "update the auth code" has not. Write the
   former.

3. **Pick a clear title** (`type(scope): summary`, ≤50 chars), e.g.
   `feat(auth): add jwt refresh middleware`. Non-PR handoff stages keep the
   worktree's existing branch — the title only names the task.

4. **Run the handoff** from the worktree root:
   ```bash
   sybra-cli handoff \
     --title "feat(auth): add jwt refresh middleware" \
     --body  "One-paragraph problem statement + the research context Sybra needs." \
     --plan-file ./.sybra-handoff-plan.md
   ```
   - `--stage review` / `ready-review` / `agentic-review` to skip implementation
     (you already coded it); `--stage testing` to skip implementation and
     review; `--stage ready-pr` to skip directly to PR creation/update;
     `--stage pr --pr N` to hand off an existing PR. Default is
     `--stage implement`.
   - `--status STATUS` is mutually exclusive with `--stage` and creates a manual
     task in that exact status without workflow dispatch.
   - `--plan-file` matters most for `--stage implement`; for later stages it is
     optional context.
   - `--worktree-dir` defaults to the current directory; pass it explicitly if you
     run the command from elsewhere.
   - `--project` is derived from the origin remote; pass `--project owner/repo`
     only if derivation fails (the command will tell you).

5. **Report back** the printed task id and that Sybra is now implementing in this
   worktree. The user can watch the changes land here live (e.g. in Orca's diff
   view).

## Failure handling

- **"project … not registered"** — register it first, then re-run:
  ```bash
  sybra-cli project create --url "$(git remote get-url origin)"
  ```
- **Branch is the default branch** — create/switch to a feature branch before
  handing off; Sybra must not commit to `main`.
- **Sybra not running** — the task is created but stays at `todo` until the local
  Sybra app/server picks up the `task.created` event. Start Sybra, no re-run
  needed.

## Notes

- Nothing about the handoff modifies your working tree — Sybra's agent does the
  editing once it starts. Commit or stash anything you want preserved as-is first
  if it matters.
- The handoff is one-directional. To stop it, manage the task in Sybra
  (`sybra-cli update <id> --status cancelled`).
