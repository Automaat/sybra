---
name: why-human
description: Diagnose why Sybra tasks are stuck in human-required, find the root cause, file a GitHub issue (or a scrubbed local task for work-typed projects) documenting it, and unblock the task back into the workflow. Use when asked to "check why tasks need human", "find root cause of stuck tasks", "unblock human-required tasks", or "file an issue and get this task moving again". Complements — does not duplicate — the automated human-review agent that already fires on the human-required transition. Works under Claude Code, Codex, and Copilot — every step is a plain `sybra-cli`/`gh`/`git` Bash call, no provider-specific tooling.
allowed-tools: Bash
user-invocable: true
argument-hint: "[task-id]"
---

# Why Human

Root-cause tasks parked in `human-required`, file the finding, and unblock them.
Sybra already runs an automated reviewer (`humanReviewHandler`,
`internal/sybra/app_human_review.go`) the instant a task hits `human-required` —
it inspects the task and agent runs, decides `human` (genuine, needs a person)
or `sybra_bug` (files an issue and flips the task to `blocked`). This skill is
the **on-demand, manual** complement for the cases that handler doesn't close
out: tasks parked before it existed, tasks it already labeled `human` where you
want a deeper second look, or machines where `human_review.enabled` is off or
rate-limited. Never re-diagnose a task the automation already flagged
`sybra_bug` — it's already `blocked` with an issue linked, not sitting in
`human-required` anymore.

## Process

1. **List candidates.**
   ```bash
   sybra-cli --json list --status human-required
   ```
   If the user named a specific task id, skip straight to step 2 for that one.

2. **Check whether the automated reviewer already rendered a verdict** for this
   task, so you don't redo its work:
   ```bash
   sybra-cli --json get <id>
   ```
   Look at `agentRuns` for an entry with `role: human-review` and read its
   `verdict` (`"human"` | `"sybra_bug"`) and `verdictRendered` fields directly —
   don't go hunting for a fenced block in `result`, the app persists the
   decision as structured fields for exactly this reason.
   - `verdict: sybra_bug` **and** `verdictRendered: true` — the side effects
     (issue filed / local task created, status flipped to `blocked`) already
     ran. If the task is still `human-required` here, something else moved it
     back after the fact; investigate that instead of re-filing.
   - `verdict: sybra_bug` **and** `verdictRendered: false` — filing failed
     partway (see the `## Auto-review verdict` note appended to the body for
     the error) and the task is genuinely still stuck. This is squarely this
     skill's job: retry the diagnosis and filing yourself.
   - `verdict: human` — the automation only confirmed a person is needed, it
     didn't explain why in enough depth to unblock. Fair game to re-examine.

3. **Read what actually happened.** Pull together:
   - The task `body`, `statusReason`, and recent `agentRuns` (prompts + results)
     from the `get --json` output above — this is almost always enough.
   - Artifacts, if the task has any:
     ```bash
     sybra-cli --json artifact list <id>
     sybra-cli --json artifact get <id> <name>
     ```
   - The worktree/branch state if a PR or diff is involved (`git log`, `gh pr
     view <n>` if `prNumber` is set on the task).

   Classify the root cause. Common buckets:
   - **Permission/tooling denial** — agent needed a tool call it wasn't allowed
     (missing `allowed_tools` entry, `require_permissions` blocking headless).
   - **Ambiguous or underspecified task** — the agent genuinely couldn't decide
     between valid approaches; this is a judgment call, not a bug.
   - **Failing test / CI / build the agent couldn't resolve** within its turn
     budget or tool access.
   - **Merge conflict or stale worktree** requiring a human decision on which
     side wins.
   - **Sybra defect** — a workflow step, prompt, or gate misbehaved (wrong
     status transition, a tool that should have been allowed wasn't, a prompt
     that led the agent astray). This is the only bucket that warrants filing
     an issue against Sybra itself.

   If the root cause is a genuine judgment call (ambiguous spec, a real
   decision only a human can make) rather than a Sybra defect, do not force an
   issue into existence — append a clear note explaining the decision needed
   and stop; leave the task in `human-required` for the person to actually
   decide. Filing an issue is for the "Sybra defect" bucket only.

4. **File the finding — respect Work-Data Confidentiality.** Before filing,
   check the task's project type, since a raw public GitHub issue is only safe
   for non-work projects:
   ```bash
   sybra-cli --json get <id>              # read .projectId
   sybra-cli --json project get <projectId>   # read .type: "work" | "pet"
   ```
   - **`pet` project, confirmed** — file directly on the public repo:
     ```bash
     gh issue create --repo Automaat/sybra --label sybra-bug \
       --title "<short root cause summary>" \
       --body  "<root cause, evidence, and how it manifested — see below>"
     ```
   - **`work` project, no project at all, or type lookup fails** — never call
     `gh issue create`; there is no CLI hook
     into the app's regex scrubber (`internal/scrub`, `App.workScrubContextForTask`
     is Go-only), so you are the scrubber. File a local scrubbed task instead,
     writing the body yourself with every work-repo identifier removed (GitHub
     URLs/branch names/commit SHAs from work repos, ticket keys, internal
     hostnames, customer names, code snippets) — describe the Sybra defect
     abstractly, the same way `fileLocalScrubbed` would:
     ```bash
     sybra-cli --json create --title "<short root cause summary>" \
       --tags "sybra-bug,scrubbed" \
       --body "<scrubbed root cause + evidence, no work-repo identifiers>"
     ```
   Issue/task body should cover: what the agent was trying to do, what broke,
   the concrete evidence (log line, tool error, failing check), and the
   suspected fix location if you found one. Don't speculate past the evidence.

5. **Unblock the task**, referencing what you filed. The default path is to
   flip it back into the workflow:
   ```bash
   sybra-cli --json update <id> --status todo \
     --status-reason "root cause: <one line>; see <issue-url-or-task-id>" \
     --issue <issue-url>   # omit --issue when you filed a local scrubbed task instead
   ```
   `update --issue` writes only the task's `ref_issue` annotation field, never
   the canonical `issue` field consumed for PR auto-close — it doesn't file or
   link anything by itself, so it's safe to attach alongside either filing
   path above.

   Two exceptions, both from prior painful experience with this exact flow:
   - **Do not use `link-pr`** here — `link-pr <id> <pr-number>` does exactly
     one thing: it sets `prNumber` and auto-advances the task to `in-review`.
     It does not attach the filed issue and it's wrong whenever the actual
     unblock is "retry the work," not "review a PR."
   - **"Ship as-is" resolutions** (the task already has a PR open and the
     verdict is "the PR is fine, stop blocking on the human-required gate") —
     keep the status at `human-required` and just attach the PR number:
     ```bash
     sybra-cli --json update <id> --status human-required --pr <n> \
       --status-reason "ship as-is: <one line>; see <issue-url-or-task-id>"
     ```
     Setting it back to `todo` here would cause Sybra to redo work that's
     already done; the outbound PR-review flow is what should pick this up
     next, and it keys off `prNumber` + `human-required`, not `todo`.

6. **Report back** per task: root cause bucket, where you filed it (GH issue
   URL, or local task id for scrubbed work tasks), and the new status. If a
   task turned out to be a genuine human decision (step 3's "stop" case), say
   so plainly instead of forcing steps 4-5 to happen anyway.

## Constraints

- Never file a public GitHub issue for a `work`-typed task's content, even
  abstracted — if you're unsure whether a project is work-typed, treat it as
  work-typed and use the local scrubbed-task path.
- Don't re-run this on a task the automated reviewer already resolved
  (`sybra_bug` verdict, task now `blocked`) — that's a duplicate, not a gap.
- Don't invent a Sybra defect to justify filing an issue; tasks whose root
  cause is a genuine ambiguous decision stay `human-required` with a note, not
  an issue.
