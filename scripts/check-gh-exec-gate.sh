#!/usr/bin/env bash
# Ban new code that shells out to `gh` outside internal/github's single gated
# constructor (github.Run / github.RunWithEnv, both backed by ghGate.execute).
#
# ghGate is what gives every gh invocation in the process shared request
# pacing, rate-limit budget bookkeeping, and the auth-health circuit breaker.
# A call site that spawns `gh` directly bypasses all three: its traffic is
# invisible to the shared budget and it keeps hammering `gh` during an outage
# the rest of the process has already backed off from. This bit twice before
# a shared gate existed at all (the monitor issue sink and
# review.findMergedPRByBranch both bypassed it independently — see #2496) —
# this check exists so a third occurrence fails here instead of on a live
# rate-limit incident.
#
# This is a literal-string grep, not an AST/data-flow check: it flags
# exec.Command/exec.CommandContext invocations whose first argument is the
# literal "gh". It cannot catch every indirection (e.g. building the binary
# name in a variable), so it narrows the risk rather than closing it.

set -euo pipefail

cd "$(dirname "$0")/.."

# Files allowed to spawn `gh` directly:
#   - internal/github/client.go: RunWithEnv, the sole gated constructor. Every
#     other gh call in the process (in this package or any other) must route
#     through it (or the Run wrapper) instead of shelling out on its own.
#   - internal/github/pr_create.go: createPRRunner already wraps its
#     exec.CommandContext call in ghGate.execute directly (it needs cmd.Dir,
#     which RunWithEnv doesn't expose) — gated by construction, just not via
#     the shared helper.
#   - scripts/check-gh-exec-gate.sh (this file): the patterns below are grep
#     targets, not a gh invocation.
ALLOWLIST=(
  "internal/github/client.go"
  "internal/github/pr_create.go"
  "scripts/check-gh-exec-gate.sh"
)

is_allowlisted() {
  local f="$1"
  for allowed in "${ALLOWLIST[@]}"; do
    [[ "${f}" == "${allowed}" ]] && return 0
  done
  return 1
}

fail=0

files=()
while IFS= read -r -d '' file; do
  case "${file}" in
    *.go)
      files+=("${file}")
      ;;
  esac
done < <(git ls-files -z --cached --others --exclude-standard)

# Matches exec.Command(...) / exec.CommandContext(...) where "gh" is the
# literal command name — the first argument after an optional leading ctx.
PATTERN='exec\.Command(Context)?\((context\.[A-Za-z]+\(\)|[A-Za-z0-9_]+, *)?"gh"'

if ((${#files[@]} > 0)); then
  while IFS= read -r hit; do
    [[ -z "${hit}" ]] && continue
    f="${hit%%:*}"
    if is_allowlisted "${f}"; then
      continue
    fi
    if ((fail == 0)); then
      echo "ERROR: direct \`gh\` subprocess spawn outside internal/github's gated Run/RunWithEnv (#2496)." >&2
      echo "Route through github.Run(ctx, args...) (or RunWithEnv for a custom env) instead of exec.Command(Context) so this traffic isn't invisible to the shared rate budget." >&2
      echo >&2
    fi
    fail=1
    echo "  ${hit}" >&2
  done < <(grep -nHE "${PATTERN}" "${files[@]}" 2>/dev/null | sed 's#^\./##' || true)
fi

if ((fail != 0)); then
  exit 1
fi

echo "check-gh-exec-gate: ok"
