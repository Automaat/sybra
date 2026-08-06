#!/usr/bin/env bash
# Fail if ignored files are still tracked, or if known runtime/build output
# roots get committed at all. Generated source that is intentionally checked in
# (for example Wails bindings and copied skills) is allowed because it is
# neither ignored nor under these explicit output roots.

set -euo pipefail

cd "$(dirname "$0")/.."

fail=0

while IFS= read -r path; do
  [[ -z "${path}" ]] && continue
  echo "::error file=${path}::tracked file matches .gitignore. Remove it from the index so ignore rules can protect runtime/build output." >&2
  fail=1
done < <(git ls-files -ci --exclude-standard)

while IFS= read -r path; do
  [[ -z "${path}" ]] && continue
  echo "::error file=${path}::tracked runtime/build artifact under a banned path (.tmp-sybra-home*, .tmp-test-fakebin/, bin/, frontend/.vite/)." >&2
  fail=1
done < <(
  git ls-files | awk '
    $0 ~ /^\.tmp-sybra-home/ ||
    $0 ~ /^\.tmp-test-fakebin\// ||
    $0 ~ /^bin\// ||
    $0 ~ /^frontend\/\.vite\// { print }
  '
)

if [[ "${fail}" -eq 0 ]]; then
  echo "repo-hygiene OK — no tracked ignored files or banned runtime/build artifacts found"
fi

exit "${fail}"
