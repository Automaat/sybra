#!/usr/bin/env bash
# Keep internal/fsutil the single home for atomic file writes.
#
# fsutil.AtomicWrite/AtomicWriteMode do the whole sequence: write a temp file
# in the target directory, chmod, fsync the file, rename, then fsync the parent
# directory. Every hand-rolled copy this repo grew skipped that last step, so a
# crash could lose the rename even though the data was durable — and each one
# re-derived its own temp-naming and cleanup rules. Record stores on raw
# os.WriteFile were worse: a crash mid-write left a truncated JSON record.
#
# Test files are included: a test that hand-rolls the sequence is usually
# asserting durability it does not actually get, and the two legitimate cases
# are allowlisted in the checker.

set -euo pipefail

cd "$(dirname "$0")/.."

files=()
while IFS= read -r -d '' file; do
  case "${file}" in
    *.go) files+=("${file}") ;;
  esac
done < <(git ls-files -z --cached --others --exclude-standard)

if ((${#files[@]} > 0)); then
  go run ./scripts/checkatomicwrite "${files[@]}"
fi
