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

if ((${#files[@]} > 0)); then
  go run ./scripts/checkgitexec "${files[@]}"
fi

echo "check-git-exec-gate: ok"
