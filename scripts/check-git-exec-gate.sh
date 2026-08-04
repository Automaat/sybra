#!/usr/bin/env bash
# Keep internal/gitexec as the single production boundary for Git subprocesses.
# Domain packages own policy (locks, timeouts, recovery, authentication), while
# gitexec owns process construction, non-interactive defaults, diagnostics, and
# cancellation. A direct spawn outside that boundary silently loses those
# mechanics and recreates the fragmentation tracked by #3006.
#
# Test files may execute Git directly to construct real repository fixtures.
# The fake-provider binaries are test infrastructure too, but their Go-level
# Git calls still use gitexec; shell-embedded Git in fake scenarios is outside
# this Go constructor check and must remain confined to cmd/fake-*.

set -euo pipefail

cd "$(dirname "$0")/.."

# The sole production package allowed to construct a Git process. Its current
# implementation resolves the executable through a variable, but keeping the
# boundary explicitly allowlisted makes the architectural exception clear if
# its constructor later uses the literal again.
ALLOWLIST=(
  "internal/gitexec/gitexec.go"
)

is_allowlisted() {
  local file="$1"
  for allowed in "${ALLOWLIST[@]}"; do
    [[ "${file}" == "${allowed}" ]] && return 0
  done
  return 1
}

files=()
while IFS= read -r -d '' file; do
  case "${file}" in
    *_test.go)
      ;;
    *.go)
      files+=("${file}")
      ;;
  esac
done < <(git ls-files -z --cached --others --exclude-standard)

# Detect both exec.Command("git", ...) and
# exec.CommandContext(ctx, "git", ...). The optional leading argument is the
# context expression; [^,]+ intentionally accepts selectors and calls such as
# context.Background(). This is a deterministic literal guard, not a Go AST or
# data-flow analysis, so indirection through a binary-name variable is reviewed
# separately rather than pretending this grep proves more than it does.
PATTERN='exec\.Command(Context)?\(([^,]+,[[:space:]]*)?"git"([[:space:]]*,|\))'

fail=0
if ((${#files[@]} > 0)); then
  while IFS= read -r hit; do
    [[ -z "${hit}" ]] && continue
    file="${hit%%:*}"
    if is_allowlisted "${file}"; then
      continue
    fi
    if ((fail == 0)); then
      echo "ERROR: production Git subprocess spawn outside internal/gitexec." >&2
      echo "Route the operation through gitexec and keep only domain policy at the caller." >&2
      echo >&2
    fi
    fail=1
    echo "  ${hit}" >&2
  done < <(grep -nHE "${PATTERN}" "${files[@]}" 2>/dev/null | sed 's#^\./##' || true)
fi

if ((fail != 0)); then
  exit 1
fi

echo "check-git-exec-gate: ok"
