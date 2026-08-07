#!/usr/bin/env bash
# Guard the four consolidation boundaries established by #3078/#3079/#3080/#3081.
# The syntax-aware checker owns the exact exception ledger and rejects both new
# copies and stale exceptions. Tests are included for every primitive; their
# intentional wire-format fixtures must be listed in the exact exception ledger.

set -euo pipefail

cd "$(dirname "$0")/.."

# Prove the scanner still catches each forbidden shape before trusting its
# repository-wide result.
go test ./scripts/checkconsolidated

files=()
while IFS= read -r -d '' file; do
  case "${file}" in
    *.go) files+=("${file}") ;;
  esac
done < <(git ls-files -z --cached --others --exclude-standard)

if ((${#files[@]} > 0)); then
  go run ./scripts/checkconsolidated "${files[@]}"
fi
