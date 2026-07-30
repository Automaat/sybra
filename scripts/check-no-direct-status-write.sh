#!/usr/bin/env bash
# Ban new code that mutates a task's Status field via task.Update{...} passed
# directly to Manager.Update/UpdateFn, bypassing the transition API
# (Manager.Apply / Manager.ApplyStatusEffect, internal/task/transition.go).
# Direct writers can each combine status mutation, audit emission, dispatch
# effects, and retries differently, and none of them enforce a
# precondition — a later poller can silently overwrite a newer decision. See
# #2726.
#
# This is a literal-string/proximity grep, not a full AST/data-flow check:
# it flags a `task.Update{` composite literal that also sets a `Status:`
# field within the next few lines — the pattern every current direct writer
# in this repo actually uses. It cannot see a Status field assigned through
# an intermediate variable built far away from the call site, so it narrows
# the risk rather than closing it, same as check-no-home-fallback.sh and
# check-gh-exec-gate.sh.
#
# Apply/ApplyFn/ApplyStatusEffect callers are unaffected: TransitionIntent and
# StatusEffect take ToStatus as a plain field (never a task.Update{Status:
# ...} literal) precisely so a migrated call site never trips this scan —
# construct the Extra/Update value inline as the intent's Extra field (or
# pull Status out into a local before calling Apply) rather than building a
# separate `task.Update{...}` variable ahead of the call, or the 200-char
# scan window can still catch the unrelated `ToStatus`/`ExpectedStatus` text
# that follows it.
#
# Every previously-allowlisted direct writer (recovery, watchdog, monitor,
# review, umbrella, the app_*/svc_* service layer, cmd/sybra-cli) has been
# migrated onto Manager.Apply/ApplyFn/ApplyStatusEffect/CreateWithStatus (see
# #2726) — there is no more incremental-migration debt, so the allowlist
# below is empty and this scan is now a strict repo-wide gate: internal/task
# is the only package allowed to construct a Status-bearing task.Update{}
# literal.

set -euo pipefail

cd "$(dirname "$0")/.."

# Files allowed to hold a `task.Update{...Status: ...}` literal, beyond
# internal/task/** (the transition implementation itself — Manager.Apply,
# Manager.Update/UpdateFn/CreateFull, ApplyStatusEffect — and its own tests
# build these literals as the mechanism's raw material). Empty: every
# production call site now routes through the transition API.
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
# $1 that also sets a `Status:` field within the next 6 lines (or before the
# literal closes, whichever comes first) — the shape every current direct
# writer uses (`task.Update{\n\tStatus: ...\n\t...\n}`), gofmt'd across at
# most a handful of lines for these call sites in practice.
scan_file() {
  awk '
    { lines[FNR] = $0; maxline = FNR }
    END {
      for (i = 1; i <= maxline; i++) {
        if (lines[i] ~ /task\.Update\{/) {
          found = 0
          for (j = i; j <= i + 6 && j <= maxline; j++) {
            if (lines[j] ~ /Status:/) { found = 1; break }
            if (j > i && lines[j] ~ /^[\t ]*\}/) { break }
          }
          if (found) print i
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
    echo "::error file=${file},line=${lineno}::direct task.Update{Status: ...} write outside the transition API (see #2726). Route this through tasks.Apply(ctx, task.TransitionIntent{...}) (or ApplyStatusEffect for a poller/watchdog-style durable effect) instead of Manager.Update/UpdateFn." >&2
    fail=1
  done < <(scan_file "${file}")
done

if [[ "${fail}" -eq 0 ]]; then
  echo "no-direct-status-write OK — no new direct task-status writes found outside the transition API / allowlist"
fi

exit "${fail}"
