---
name: claude-feature-adoption
description: Review what shipped in Claude Code since the last adoption analysis and file an umbrella issue (☂️) + one subissue per adoptable feature, each grounded in how Sybra actually drives Claude headless. Use when asked to "check what's new in Claude Code", "adopt new Claude features", or to refresh the Claude Code adoption tracker. For the Codex CLI, use codex-feature-adoption instead.
argument-hint: "[--from <version>] [--dry-run]"
user-invocable: true
allowed-tools:
  - Agent
  - Read
  - Grep
  - Glob
  - Bash
  - WebFetch
---

# Claude Code Feature Adoption

Find what shipped in **Claude Code** since Sybra last did an adoption pass, decide what's worth adopting (grounded in how Sybra actually invokes the `claude` CLI), and file an umbrella issue with one subissue per item.

This is the **Claude** variant. For the OpenAI **Codex** CLI, use `codex-feature-adoption` — same workflow, different changelog source and grounding points.

**Repo:** `Automaat/sybra` (public personal project). Everything this skill writes is about Claude Code *public* features — safe to file. Never paste work-repo content (see the Work-Data Confidentiality rule in the root `CLAUDE.md`); only reference Sybra's own code.

## Phase 1 — Establish the baseline (what's already covered)

1. **Find the last umbrella.** List prior adoption trackers, newest first:
   ```bash
   gh issue list --repo Automaat/sybra --state all --search '"Adopt Claude Code" in:title' \
     --json number,title,state,createdAt --limit 20
   ```
   The most recent `☂️ Adopt Claude Code vX-Y features` gives the **floor** = its upper version `Y`. If none exists, ask the user for a starting version or default to ~1 month back.

2. **Read the last umbrella + its subissues** so you don't re-file already-adopted items:
   ```bash
   gh issue view <last-umbrella> --repo Automaat/sybra --json title,body
   gh api graphql -f query='{ repository(owner:"Automaat",name:"sybra"){ issue(number:<last-umbrella>){ subIssues(first:50){ nodes{ number title state } } } } }' \
     -H "GraphQL-Features: sub_issues"
   ```
   Closed subissues = already adopted (skip). Open subissues = **carryovers** to list in the new umbrella, noting any the new changelog *reinforces* or *unblocks*.

3. **Current installed version** = the ceiling:
   ```bash
   claude --version
   ```
   New range = **(floor + 1) → installed**. Override the floor with `--from <version>` if passed.

## Phase 2 — Read the changelog for the new range only

- **Primary source:** the local cache `~/.claude/cache/changelog.md`. Version headers are `## X.Y.Z`, **newest first**. Find the line for the floor version and read everything *above* it up to the installed version. It's large — read in slices with `offset`/`limit`; never dump the whole file.
  ```bash
  grep -n '^## ' ~/.claude/cache/changelog.md | head -80   # locate the range line numbers
  ```
- **Fallback** (cache missing/stale): WebFetch the official changelog — `https://raw.githubusercontent.com/anthropics/claude-code/main/CHANGELOG.md` (or the `@anthropic-ai/claude-code` npm page).

## Phase 3 — Ground the analysis in Sybra's code

A changelog entry only matters if it touches how Sybra invokes or manages `claude`. **Re-verify against the current tree every run** (the map drifts) — spawn `Explore` agents in parallel, or read directly. Known integration points (verify, don't trust):

| Concern | Where to look |
| :-- | :-- |
| Headless `claude -p` flags | `internal/agent/runner_headless_invocation.go` (the non-codex branch: `-p`, `--output-format stream-json --verbose`, `--resume`, `--allowedTools` / `--dangerously-skip-permissions`, `--model`) |
| Subprocess env vars | same file — `BASH_*_TIMEOUT_MS`, `CLAUDE_CODE_FORK_SUBAGENT`; check what's **not** set (fallback model, retry watchdog, effort, session id, AI_AGENT) |
| NDJSON stream parsing | `internal/agent/runner_headless.go`, `internal/agent/stream.go` — which event types (`system`/`init`, `assistant`, `user`, `result`) and fields (`plugin_errors`) are handled |
| Resume / session id | `manager_run.go`, `runner_headless.go` (session id captured from `init`) |
| Approval hook | `internal/agent/runner_convo.go` + `internal/agent/approval_server.go` (HTTP `PreToolUse` via `--settings`) |
| Scrub / confidentiality | `internal/scrub`, `internal/sybra/app_human_review.go`, `monitor_sink.go` |
| Model list | `internal/sybra/svc_info.go`, `manager_run.go` `normalizeModel` (Claude side is hardcoded aliases) |
| Reviews | `internal/sybra/app_reviews_inbound.go` (`/staff-code-review`, `/fix-review-auto`) |
| Server / unattended | root `CLAUDE.md` "Server Deployment" — unattended headless agents on the `synapse` LXC |

For each candidate feature, state **exactly where it plugs in** (file:line) and the **effort**. Drop anything UI-only, terminal-rendering, Windows-specific, or already adopted.

## Phase 4 — Bucket the findings

Mirror the prior umbrellas (#685, #1019):

- **High value** — clean adopts that improve server resilience, reliability, or cost at low effort (e.g. `--fallback-model`, `CLAUDE_CODE_RETRY_WATCHDOG`, `--json-schema` structured output).
- **Medium** — worthwhile but more effort or a behavior change (e.g. auto mode vs `--dangerously-skip-permissions`, `--effort` per role, disabling bundled skills, model-picker additions).
- **Smaller** — confidentiality/security/cleanup (e.g. `attribution.sessionUrl`, `sandbox.credentials`, free bug-fix-on-bump).
- **Parallel groups** — Group A = changes that all touch the same file (e.g. `runner_headless_invocation.go`) so they serialize; Group B = independent, parallelizable.
- **Carryovers** — still-open subissues from prior umbrellas; flag which the new changelog reinforces/unblocks.
- **Strategic watch** — large/convergent items, not drop-ins (e.g. native `claude agents`/`--bg`, OTEL signals for the monitor).
- **Skipped** — features evaluated and rejected, with the one-line reason.

Present this to the user before filing. If invoked `--dry-run`, stop here.

## Phase 5 — File the umbrella + subissues

1. **Umbrella** (title prefixed with ☂️, per the global git rules):
   ```bash
   gh issue create --repo Automaat/sybra \
     --title "☂️ Adopt Claude Code vX-Y features" \
     --body-file <umbrella.md>
   ```
   Body = the buckets above + parallel groups + carryovers (with `#refs`) + a `## Source` line naming the changelog range. Write bodies to files in the scratchpad; never use `-F`/heredoc-to-`--body` for long text.

2. **One subissue per adoptable item.** Each body: the bucket + parallel group, the Sybra grounding (file refs), concrete steps, and a `Source: Claude Code CHANGELOG vX.Y.Z` line. Create, then link to the umbrella:
   ```bash
   gh-add-subissue <umbrella#> <child#> Automaat/sybra
   ```
   (`gh-add-subissue` is in `~/.local/bin`; it needs the `GraphQL-Features: sub_issues` header, which it sets.)

3. **Verify the links landed** — `gh-add-subissue` occasionally drops one:
   ```bash
   gh api graphql -f query='{ repository(owner:"Automaat",name:"sybra"){ issue(number:<umbrella#>){ subIssues(first:50){ totalCount } } } }' \
     -H "GraphQL-Features: sub_issues"
   ```
   If `totalCount` < number created, re-run `gh-add-subissue` for the missing child.

4. **Report** the umbrella URL and the subissue list, and recommend the highest-leverage / lowest-effort starting pair.

## Conventions

- Commit/issue titles: conventional-commit style where applicable; umbrella titles lead with ☂️.
- No work-repo identifiers anywhere. No AI attribution. No `#`-ref noise that title hooks reject.
- Keep subissue bodies terse and grounded — a generic body ("adopt feature X") is a failure; name the file it lands in.
