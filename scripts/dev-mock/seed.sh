#!/usr/bin/env bash
# Seed a SYBRA_HOME with mock tasks + projects spanning every pipeline state,
# so the web UI has rich data to iterate against. Idempotent: wipes and
# rewrites tasks/ and projects/ on each run.
#
# Usage: SYBRA_HOME=/path/to/home scripts/dev-mock/seed.sh
set -euo pipefail

: "${SYBRA_HOME:?set SYBRA_HOME to the isolated dev home}"

TASKS="$SYBRA_HOME/tasks"
PROJECTS="$SYBRA_HOME/projects"
CLONES="$SYBRA_HOME/clones"
LOGS="$SYBRA_HOME/logs"

rm -rf "$TASKS" "$PROJECTS"
mkdir -p "$TASKS" "$PROJECTS" "$CLONES" "$LOGS"

# Fixed timestamps keep the seed deterministic (no churn between runs).
NOW="2026-07-03T09:00:00Z"
EARLIER="2026-07-02T14:30:00Z"
OLDER="2026-06-28T11:00:00Z"

# ---------------------------------------------------------------------------
# Projects
# ---------------------------------------------------------------------------
proj() { # id owner repo type
  local owner="$1" repo="$2" type="$3"
  cat >"$PROJECTS/${owner}--${repo}.yaml" <<EOF
id: ${owner}/${repo}
name: ${repo}
owner: ${owner}
repo: ${repo}
url: https://github.com/${owner}/${repo}
clone_path: ${CLONES}/${owner}/${repo}.git
type: ${type}
status: ready
worktree_base_ref: fresh
created_at: ${OLDER}
updated_at: ${NOW}
EOF
}

proj "Automaat" "sybra" "pet"
proj "Automaat" "baratie" "pet"
proj "acme" "internal-api" "work"

# ---------------------------------------------------------------------------
# Tasks
# Args: id status title priority project tags body [extra yaml lines...]
# ---------------------------------------------------------------------------
task() {
  local id="$1" status="$2" title="$3" prio="$4" proj="$5" tags="$6" body="$7"
  shift 7
  {
    echo "---"
    echo "id: ${id}"
    echo "title: ${title}"
    echo "status: ${status}"
    echo "task_type: normal"
    echo "agent_mode: headless"
    echo "allowed_tools: []"
    echo "tags: [${tags}]"
    [ -n "$proj" ] && echo "project_id: ${proj}"
    [ -n "$prio" ] && echo "priority: ${prio}"
    for line in "$@"; do echo "$line"; done
    echo "created_at: ${EARLIER}"
    echo "updated_at: ${NOW}"
    echo "---"
    echo "## Description"
    echo ""
    echo "${body}"
  } >"$TASKS/${id}.md"
}

# --- new / todo (backlog) --------------------------------------------------
task task-new001 new "Add dark-mode toggle to settings panel" high "Automaat/sybra" "frontend,ui" \
  "Users want a manual light/dark override instead of following the OS theme."

task task-todo01 todo "Cache GitHub rate-limit responses" medium "Automaat/sybra" "backend,github" \
  "Avoid re-fetching /rate_limit on every poll; cache for 30s."

task task-todo02 todo "Document the web-mode dev workflow" low "Automaat/sybra" "docs" \
  "Explain proxy + mock backend for GUI iteration."

# --- planning / plan-review ------------------------------------------------
task task-plan01 planning "Redesign the task detail sidebar" medium "Automaat/sybra" "frontend,ui,design" \
  "Split metadata, agent runs, and diff into collapsible sections."

task task-plan02 plan-review "Introduce a project archive state" low "Automaat/baratie" "backend" \
  "Let projects be archived without deleting their clone." \
  "review_phase: awaiting-human"

# --- in-progress (active agent, with prior resumed runs) -------------------
task task-prog01 in-progress "Fix flaky watcher debounce on rapid saves" high "Automaat/sybra" "backend,bug" \
  "fsnotify fires twice on some editors; debounce window too small." \
  "branch: sybra/task-prog01-fix-watcher" \
  "worktree_dir: ${SYBRA_HOME}/worktrees/task-prog01" \
  "agent_runs:" \
  "  - agent_id: agent-prog01-triage" \
  "    role: triage" \
  "    mode: headless" \
  "    provider: claude" \
  "    model: claude-haiku-4-5-20251001" \
  "    reasoning_effort: low" \
  "    state: stopped" \
  "    started_at: ${OLDER}" \
  "    cost_usd: 0.0031" \
  "    one_shot: true" \
  "    result: Classified as backend bug; assigned to Automaat/sybra." \
  "  - agent_id: agent-prog01-a" \
  "    role: implementation" \
  "    mode: headless" \
  "    provider: claude" \
  "    model: claude-opus-4-8" \
  "    reasoning_effort: high" \
  "    state: stopped" \
  "    started_at: ${EARLIER}" \
  "    cost_usd: 0.2140" \
  "    session_id: sess-prog01-a" \
  "    head_sha: 9f3c1ab" \
  "    result: First attempt stalled on an unrelated test flake; watchdog stopped the loop." \
  "  - agent_id: agent-prog01-b" \
  "    role: implementation" \
  "    mode: headless" \
  "    provider: claude" \
  "    model: claude-opus-4-8" \
  "    reasoning_effort: high" \
  "    state: running" \
  "    started_at: ${NOW}" \
  "    cost_usd: 0.0421" \
  "    session_id: sess-prog01-a"

task task-prog02 in-progress "Add keyboard nav to the command palette" medium "Automaat/baratie" "frontend" \
  "Arrow keys + enter to select; escape to close." \
  "branch: sybra/task-prog02-palette-nav" \
  "agent_runs:" \
  "  - agent_id: agent-prog02-a" \
  "    role: implementation" \
  "    mode: headless" \
  "    provider: codex" \
  "    model: gpt-5-codex" \
  "    state: running" \
  "    started_at: ${NOW}"

# --- review lane -----------------------------------------------------------
task task-rev01 ready-review "Extract scrub blocklist builder into helper" low "Automaat/sybra" "backend,refactor" \
  "Deduplicate blocklist derivation across auto-sources." \
  "branch: sybra/task-rev01-scrub-helper" \
  "reviewed: false"

task task-rev02 in-review "Rate-limit the Todoist poller" medium "Automaat/sybra" "backend,integration" \
  "Back off when Todoist returns 429." \
  "branch: sybra/task-rev02-todoist-backoff" \
  "review_phase: reviewing" \
  "agent_runs:" \
  "  - agent_id: agent-rev02-plan" \
  "    role: plan" \
  "    mode: headless" \
  "    provider: claude" \
  "    model: claude-opus-4-8" \
  "    reasoning_effort: medium" \
  "    state: stopped" \
  "    started_at: ${OLDER}" \
  "    cost_usd: 0.0512" \
  "    result: 'Plan: wrap the poller in a token-bucket limiter with Retry-After support.'" \
  "  - agent_id: agent-rev02-impl" \
  "    role: implementation" \
  "    mode: headless" \
  "    provider: claude" \
  "    model: claude-opus-4-8" \
  "    reasoning_effort: high" \
  "    state: done" \
  "    started_at: ${EARLIER}" \
  "    cost_usd: 0.1832" \
  "    head_sha: 4b8e0d2" \
  "    result: Implemented exponential backoff with jitter." \
  "  - agent_id: agent-rev02-review1" \
  "    role: review" \
  "    mode: headless" \
  "    provider: codex" \
  "    model: gpt-5-codex" \
  "    state: failed" \
  "    started_at: ${EARLIER}" \
  "    cost_usd: 0.0904" \
  "    verdict: changes-requested" \
  "    result: Backoff ignores the Retry-After header; jitter can exceed max delay." \
  "  - agent_id: agent-rev02-fix" \
  "    role: fix-review" \
  "    mode: headless" \
  "    provider: claude" \
  "    model: claude-opus-4-8" \
  "    state: done" \
  "    started_at: ${NOW}" \
  "    cost_usd: 0.0771" \
  "    head_sha: 7c1f9aa" \
  "    result: Honoured Retry-After; clamped jitter to max delay." \
  "  - agent_id: agent-rev02-review2" \
  "    role: review" \
  "    mode: headless" \
  "    provider: codex" \
  "    model: gpt-5-codex" \
  "    state: running" \
  "    started_at: ${NOW}"

# --- testing ---------------------------------------------------------------
task task-test01 testing "Persist window size across restarts" low "Automaat/baratie" "frontend" \
  "Store last window geometry and restore on launch." \
  "branch: sybra/task-test01-window-size" \
  "testing_cycle_started_at: ${NOW}" \
  "agent_runs:" \
  "  - agent_id: agent-test01-impl" \
  "    role: implementation" \
  "    mode: headless" \
  "    provider: claude" \
  "    model: claude-opus-4-8" \
  "    state: done" \
  "    started_at: ${OLDER}" \
  "    cost_usd: 0.0912" \
  "    head_sha: 2a7d4e1" \
  "    result: Persisted geometry to settings; restore on launch." \
  "  - agent_id: agent-test01-test1" \
  "    role: test-runner" \
  "    mode: headless" \
  "    provider: codex" \
  "    model: gpt-5-codex" \
  "    state: failed" \
  "    started_at: ${EARLIER}" \
  "    cost_usd: 0.0623" \
  "    test_outcome: product_bug" \
  "    test_failure_fingerprint: fp-c41a90" \
  "    result: Restored window opens off-screen on a disconnected external display." \
  "  - agent_id: agent-test01-fix" \
  "    role: fix-review" \
  "    mode: headless" \
  "    provider: claude" \
  "    model: claude-opus-4-8" \
  "    state: done" \
  "    started_at: ${EARLIER}" \
  "    cost_usd: 0.0455" \
  "    head_sha: b0619c3" \
  "    result: Clamp restored geometry to the current visible frame." \
  "  - agent_id: agent-test01-test2" \
  "    role: test-runner" \
  "    mode: headless" \
  "    provider: codex" \
  "    model: gpt-5-codex" \
  "    state: running" \
  "    started_at: ${NOW}"

# --- ready-pr --------------------------------------------------------------
task task-pr01 ready-pr "Add --json flag to sybra-cli config doctor" medium "Automaat/sybra" "cli" \
  "Machine-readable doctor output for skills." \
  "branch: sybra/task-pr01-doctor-json" \
  "pr_phase: opening" \
  "reviewed: true"

# --- human-required --------------------------------------------------------
task task-hr01 human-required "Migrate config schema to v2" high "Automaat/sybra" "backend,config" \
  "Ambiguous migration path for legacy todoist token field." \
  "status_reason: Needs a human decision on token migration strategy." \
  "branch: sybra/task-hr01-config-v2" \
  "agent_runs:" \
  "  - agent_id: agent-hr01-impl" \
  "    role: implementation" \
  "    mode: headless" \
  "    provider: claude" \
  "    model: claude-opus-4-8" \
  "    reasoning_effort: high" \
  "    state: stopped" \
  "    started_at: ${EARLIER}" \
  "    cost_usd: 0.3011" \
  "    protocol_violation: 'escalated: ambiguous requirement'" \
  "    result: Two viable migration paths for the legacy token; needs a human call." \
  "  - agent_id: agent-hr01-human" \
  "    role: human-review" \
  "    mode: headless" \
  "    provider: claude" \
  "    model: claude-opus-4-8" \
  "    state: stopped" \
  "    started_at: ${NOW}" \
  "    cost_usd: 0.0189" \
  "    verdict: human" \
  "    verdict_rendered: true" \
  "    one_shot: true" \
  "    result: Genuine ambiguity, not a Sybra bug — routed to human."

# --- blocked ---------------------------------------------------------------
task task-blk01 blocked "Wire renovate fixer to work projects" medium "acme/internal-api" "backend,renovate,scrubbed" \
  "Blocked on a sybra bug filed locally (work-typed, scrubbed)." \
  "blocked_by_issue: sybra-local://task-hr01" \
  "depends_on: [Automaat/sybra#1200]"

# --- done ------------------------------------------------------------------
task task-done01 "done" "Ship SSE multiplexing for web mode" high "Automaat/sybra" "frontend,backend" \
  "Single EventSource funnels all subscriptions." \
  "branch: sybra/task-done01-sse" \
  "pr_number: 1421" \
  "reviewed: true" \
  "outcome: merged" \
  "merge_commit: a1b2c3d4e5f6" \
  "closed_at: ${NOW}" \
  "agent_runs:" \
  "  - agent_id: agent-done01-triage" \
  "    role: triage" \
  "    mode: headless" \
  "    provider: claude" \
  "    model: claude-haiku-4-5-20251001" \
  "    reasoning_effort: low" \
  "    state: stopped" \
  "    started_at: ${OLDER}" \
  "    cost_usd: 0.0028" \
  "    one_shot: true" \
  "    result: Classified as frontend+backend feature." \
  "  - agent_id: agent-done01-plan" \
  "    role: plan" \
  "    mode: headless" \
  "    provider: codex" \
  "    model: gpt-5-codex" \
  "    reasoning_effort: high" \
  "    state: stopped" \
  "    started_at: ${OLDER}" \
  "    cost_usd: 0.0820" \
  "    result: 'Plan: single shared EventSource multiplexing all subscriptions.'" \
  "  - agent_id: agent-done01-plancritic" \
  "    role: plan-critic" \
  "    mode: headless" \
  "    provider: claude" \
  "    model: claude-opus-4-8" \
  "    state: stopped" \
  "    started_at: ${OLDER}" \
  "    cost_usd: 0.0345" \
  "    verdict: approve" \
  "    result: Plan sound; add reconnect/backoff on stream drop." \
  "  - agent_id: agent-done01-impl" \
  "    role: implementation" \
  "    mode: headless" \
  "    provider: claude" \
  "    model: claude-opus-4-8" \
  "    reasoning_effort: high" \
  "    state: done" \
  "    started_at: ${OLDER}" \
  "    cost_usd: 0.4211" \
  "    head_sha: 5d2e8f0" \
  "    result: Implemented shared EventSource + reconnect." \
  "  - agent_id: agent-done01-review" \
  "    role: review" \
  "    mode: headless" \
  "    provider: codex" \
  "    model: gpt-5-codex" \
  "    state: done" \
  "    started_at: ${EARLIER}" \
  "    cost_usd: 0.1002" \
  "    verdict: approve" \
  "    result: LGTM — clean multiplexing, good teardown." \
  "  - agent_id: agent-done01-test" \
  "    role: test-runner" \
  "    mode: headless" \
  "    provider: codex" \
  "    model: gpt-5-codex" \
  "    state: done" \
  "    started_at: ${EARLIER}" \
  "    cost_usd: 0.0688" \
  "    test_outcome: pass" \
  "    result: All subscriptions receive events over one connection; reconnect verified." \
  "  - agent_id: agent-done01-prfix" \
  "    role: pr-fix" \
  "    mode: headless" \
  "    provider: claude" \
  "    model: claude-opus-4-8" \
  "    state: done" \
  "    started_at: ${NOW}" \
  "    cost_usd: 0.0233" \
  "    head_sha: a1b2c3d" \
  "    result: 'Addressed Copilot nit: typed the event payload.'" \
  "  - agent_id: agent-done01-eval" \
  "    role: eval" \
  "    mode: headless" \
  "    provider: claude" \
  "    model: claude-opus-4-8" \
  "    state: stopped" \
  "    started_at: ${NOW}" \
  "    cost_usd: 0.0157" \
  "    one_shot: true" \
  "    result: 'Outcome: shipped as specified; 8 runs, \$0.87 total.'"

task task-done02 "done" "Add hadolint to CI" low "Automaat/sybra" "ci" \
  "Dockerfile linting in the lint job." \
  "pr_number: 1399" \
  "outcome: merged" \
  "closed_at: ${OLDER}"

# --- cancelled -------------------------------------------------------------
task task-cxl01 cancelled "Rewrite frontend in React" low "" "frontend" \
  "Superseded — staying on Svelte 5." \
  "outcome: wontfix" \
  "closed_at: ${OLDER}"

echo "Seeded $(find "$TASKS" -name '*.md' | wc -l | tr -d ' ') tasks + $(find "$PROJECTS" -name '*.yaml' | wc -l | tr -d ' ') projects into $SYBRA_HOME"
