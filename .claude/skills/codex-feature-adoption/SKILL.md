---
name: codex-feature-adoption
description: Review what shipped in the OpenAI Codex CLI since the last adoption analysis and file an umbrella issue (☂️) + one subissue per adoptable feature, each grounded in how Sybra actually drives Codex headless. Use when asked to "check what's new in Codex", "adopt new Codex features", or to refresh the Codex CLI adoption tracker. For Claude Code, use claude-feature-adoption instead.
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

# Codex CLI Feature Adoption

Find what shipped in the **OpenAI Codex CLI** since Sybra last did an adoption pass, decide what's worth adopting (grounded in how Sybra invokes the `codex` CLI as a provider), and file an umbrella issue with one subissue per item.

This is the **Codex** variant. For Anthropic's **Claude Code**, use `claude-feature-adoption` — same workflow shape, different changelog source and grounding points.

**Repo:** `Automaat/sybra` (public personal project). Everything this skill writes is about Codex CLI *public* features — safe to file. Never paste work-repo content (see the Work-Data Confidentiality rule in the root `CLAUDE.md`); only reference Sybra's own code.

## Phase 1 — Establish the baseline (what's already covered)

1. **Find the last umbrella.** List prior Codex adoption trackers, newest first:
   ```bash
   gh issue list --repo Automaat/sybra --state all --search '"Adopt Codex" in:title' \
     --json number,title,state,createdAt --limit 20
   ```
   The most recent `☂️ Adopt Codex CLI vX-Y features` gives the **floor** = its upper version `Y`. If none exists (first run), ask the user for a starting version, or default to ~1 month of Codex releases back.

2. **Read the last umbrella + its subissues** so you don't re-file already-adopted items:
   ```bash
   gh issue view <last-umbrella> --repo Automaat/sybra --json title,body
   gh api graphql -f query='{ repository(owner:"Automaat",name:"sybra"){ issue(number:<last-umbrella>){ subIssues(first:50){ nodes{ number title state } } } } }' \
     -H "GraphQL-Features: sub_issues"
   ```
   Closed subissues = already adopted (skip). Open subissues = **carryovers** to list in the new umbrella.

3. **Current installed version** = the ceiling:
   ```bash
   codex --version    # e.g. "codex-cli 0.142.2"
   ```
   New range = **(floor) → installed**. Override the floor with `--from <version>` if passed. Codex versions move fast (0.x.y) — expect a wide numeric range per month.

## Phase 2 — Read the changelog for the new range only

Codex has **no local changelog cache** (unlike Claude Code). Fetch it from the source repo:

- **Primary:** WebFetch `https://raw.githubusercontent.com/openai/codex/main/CHANGELOG.md`. Ask for the entries between the floor and installed versions.
- **Cross-check releases** if the changelog lags HEAD:
  ```bash
  gh release list --repo openai/codex --limit 40
  gh api repos/openai/codex/releases --jq '.[].tag_name' | head -40
  ```
  Read individual release notes with `gh release view <tag> --repo openai/codex` for anything not yet in `CHANGELOG.md`.

Focus only on the `codex exec` / CLI / sandbox / config surface — ignore IDE-extension, TUI, and unrelated changes.

## Phase 3 — Ground the analysis in Sybra's code

A changelog entry only matters if it touches how Sybra invokes or manages `codex`. **Re-verify against the current tree every run** (the map drifts) — spawn `Explore` agents in parallel, or read directly. Known integration points (verify, don't trust):

| Concern | Where to look |
| :-- | :-- |
| Headless `codex exec` flags | `internal/agent/runner_headless_invocation.go` (the `a.Provider == "codex"` branch: `exec --json --skip-git-repo-check --ignore-user-config --ignore-rules`, `-C <cwd>`, `--model`) |
| Sandbox / approvals | `codexSandboxArgs(...)` in the same file; `--dangerously-bypass-approvals-and-sandbox` (headless) vs `--sandbox workspace-write` (interactive) in `manager_run.go` |
| Stream parsing | `internal/agent/stream.go` — Codex event mapping (`thread.started`, `turn.started`, `turn.completed`, `item.started`/`item.completed`, `error`) into Sybra's StreamEvent shape |
| Sessions | Codex has **no `--resume`**; sessions persist as `~/.codex/sessions/<id>.jsonl` (resolver in `discovery.go`). Watch for any new resume/continue flag. |
| Skills | `discoverCodexSkills()` + `rewriteSkillInvocations()` (Codex has no native skills; Sybra rewrites `/skill` → `$` prompt injection) |
| Model list | `internal/sybra/svc_info.go` runs `codex debug models` (dynamic); `normalizeModel` maps aliases (sonnet/opus → gpt-5.4, haiku → gpt-5.4-mini) |
| Config | `--ignore-user-config` means Sybra-passed flags/env are the only config surface — note any new `codex exec` flag that replaces a config-file-only setting |
| Server / unattended | root `CLAUDE.md` "Server Deployment" — Codex config + hooks live in `/data/sybra/codex` (`codex_hooks = true`) |

For each candidate feature, state **exactly where it plugs in** (file:line) and the **effort**. Drop anything UI-only, TUI, IDE-extension, or already adopted.

## Phase 4 — Bucket the findings

Mirror the prior umbrellas (#685, #1019):

- **High value** — clean adopts improving headless resilience, reliability, or cost (e.g. a new resilience flag, structured-output/JSON mode, sandbox/approval granularity that lets Sybra drop blanket bypass).
- **Medium** — worthwhile but more effort or a behavior change (e.g. session resume/continue support, new sandbox modes, model-list/effort handling).
- **Smaller** — confidentiality/security/cleanup (e.g. credential-scoping, attribution, config hardening).
- **Parallel groups** — Group A = changes that all touch the codex branch of `runner_headless_invocation.go` (serialize); Group B = independent.
- **Carryovers** — still-open subissues from prior Codex umbrellas / the parity tracker (e.g. #256 "Codex provider parity").
- **Strategic watch** — large/convergent items, not drop-ins.
- **Skipped** — features evaluated and rejected, with the one-line reason.

Present this to the user before filing. If invoked `--dry-run`, stop here.

## Phase 5 — File the umbrella + subissues

1. **Umbrella** (title prefixed with ☂️, per the global git rules):
   ```bash
   gh issue create --repo Automaat/sybra \
     --title "☂️ Adopt Codex CLI vX-Y features" \
     --body-file <umbrella.md>
   ```
   Body = the buckets above + parallel groups + carryovers (with `#refs`) + a `## Source` line naming the changelog/release range. Write bodies to files in the scratchpad; never use `-F`/heredoc-to-`--body` for long text.

2. **One subissue per adoptable item.** Each body: the bucket + parallel group, the Sybra grounding (file refs), concrete steps, and a `Source: Codex CHANGELOG vX.Y.Z` (or release tag) line. Create, then link:
   ```bash
   gh-add-subissue <umbrella#> <child#> Automaat/sybra
   ```
   (`gh-add-subissue` is in `~/.local/bin`.)

3. **Verify the links landed** — `gh-add-subissue` occasionally drops one:
   ```bash
   gh api graphql -f query='{ repository(owner:"Automaat",name:"sybra"){ issue(number:<umbrella#>){ subIssues(first:50){ totalCount } } } }' \
     -H "GraphQL-Features: sub_issues"
   ```
   If `totalCount` < number created, re-run `gh-add-subissue` for the missing child.

4. **Report** the umbrella URL and the subissue list, and recommend the highest-leverage / lowest-effort starting pair.

## Conventions

- Issue titles: conventional-commit style where applicable; umbrella titles lead with ☂️.
- No work-repo identifiers anywhere. No AI attribution. No `#`-ref noise that title hooks reject.
- Keep subissue bodies terse and grounded — a generic body ("adopt feature X") is a failure; name the file it lands in.
- Codex moves fast and breaks flags between minor versions — when a changelog entry changes existing `codex exec` flags Sybra already passes, that's a **regression-risk** item, not just an adoption: flag it High.
