#!/usr/bin/env bash
# Ban new code that mutates a task's Workflow field via task.Update{...}
# passed directly to Manager.Update/UpdateFn, bypassing the transition API
# (Manager.Apply/ApplyFn, internal/task/transition.go). Mirrors
# check-no-direct-status-write.sh for the Workflow field — see #2749: a
# status write and a workflow write landing as two separate store calls
# leaves a crash window where a restart between them can persist a terminal
# task status with a still-running workflow (or vice versa).
#
# This is a literal-string/proximity grep, not a full AST/data-flow check:
# it flags a `task.Update{` composite literal that also sets a `Workflow:`
# field within the next few lines, UNLESS a `.Apply(` or `.ApplyFn(` call
# appears within a generous surrounding window — the sanctioned pattern
# (see taskAdapter.SetWorkflow/SetStatusAndWorkflow/ClaimWorkflowEffect/
# CompleteWorkflowEffect in internal/sybra/app_workflow.go) always feeds the
# literal into a TransitionIntent's Extra field, inline or via a same-block
# local variable, ending in that same call. A direct `Manager.Update`/
# `UpdateFn` writer has no such call nearby: `UpdateFn(` does not match
# `.ApplyFn(` as a substring, so a bypass isn't accidentally waved through.
# It cannot see a Workflow field assigned through a variable built far
# outside the surrounding window, so it narrows the risk rather than closing
# it, same as check-no-home-fallback.sh and check-no-direct-status-write.sh.
#
# Every previously-direct writer (recovery, the taskAdapter bridge) has been
# migrated onto Manager.Apply/ApplyFn (see #2749) — there is no more
# incremental-migration debt, so the allowlist below is empty and this scan
# is now a strict repo-wide gate: internal/task is the only package allowed
# to construct a Workflow-bearing task.Update{} literal outside the Extra
# pattern.

set -euo pipefail

cd "$(dirname "$0")/.."

# Files allowed to hold a `task.Update{...Workflow: ...}` literal outside the
# Extra pattern, beyond internal/task/** (the transition implementation
# itself, and its own tests, build these literals as the mechanism's raw
# material). Empty: every production call site now routes through the
# transition API.
ALLOWLIST=()

is_allowlisted() {
  local f="$1"
  case "${f}" in
    internal/task/*) return 0 ;;
  esac
  for allowed in "${ALLOWLIST[@]}"; do
    [[ "${f}" == "${allowed}" ]] && return 0
  done
  return 1
}

# scan_file prints the 1-based line number of each `task.Update{` literal in
# $1 that also sets a `Workflow:` field within the next 6 lines (or before
# the literal closes, whichever comes first) — the shape every current
# direct writer uses (`task.Update{\n\tWorkflow: ...\n\t...\n}`) — unless a
# `.Apply(` or `.ApplyFn(` call appears within 20 lines before or 25 lines
# after the literal, marking it (inline or via a same-block local variable)
# as a TransitionIntent.Extra field value instead of a bare
# Manager.Update/UpdateFn argument.
scan_file() {
  awk '
    { lines[FNR] = $0; maxline = FNR }
    END {
      for (i = 1; i <= maxline; i++) {
        if (lines[i] ~ /task\.Update\{/) {
          found = 0
          for (j = i; j <= i + 6 && j <= maxline; j++) {
            if (lines[j] ~ /Workflow:/) { found = 1; break }
            if (j > i && lines[j] ~ /^[\t ]*\}/) { break }
          }
          if (!found) continue
          routed = 0
          lo = i - 20; if (lo < 1) lo = 1
          hi = i + 25; if (hi > maxline) hi = maxline
          for (j = lo; j <= hi; j++) {
            if (lines[j] ~ /\.Apply\(/ || lines[j] ~ /\.ApplyFn\(/ || lines[j] ~ /\.ApplyStatusEffect\(/) { routed = 1; break }
          }
          if (!routed) print i
        }
      }
    }
  ' "$1"
}

files=()
while IFS= read -r -d '' file; do
  case "${file}" in
    *_test.go) continue ;;
    *.go) files+=("${file}") ;;
  esac
done < <(git ls-files -z --cached --others --exclude-standard)

fail=0
for file in "${files[@]}"; do
  is_allowlisted "${file}" && continue
  while IFS= read -r lineno; do
    [[ -z "${lineno}" ]] && continue
    echo "::error file=${file},line=${lineno}::direct task.Update{Workflow: ...} write outside the transition API (see #2749). Route this through tasks.Apply/ApplyFn(task.TransitionIntent{..., Extra: task.Update{Workflow: ...}}) instead of Manager.Update/UpdateFn." >&2
    fail=1
  done < <(scan_file "${file}")
done

if [[ "${fail}" -eq 0 ]]; then
  echo "no-direct-workflow-write OK — no new direct task-workflow writes found outside the transition API / allowlist"
fi

exit "${fail}"
