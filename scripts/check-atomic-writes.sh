#!/usr/bin/env bash
# Ban new hand-rolled temp-file-plus-rename writes outside internal/fsutil.
#
# fsutil.AtomicWrite/AtomicWriteMode do the whole sequence: write to a temp
# file in the target directory, chmod, fsync the file, rename, then fsync the
# parent directory. Every hand-rolled copy this repo grew skipped the
# directory sync, so a crash could leave the rename itself unreachable even
# though the data was durable — and each one re-derived its own temp-naming
# and cleanup rules. Record stores that used raw os.WriteFile were worse
# still: a crash mid-write left a truncated JSON record on disk.
#
# This is a literal-string grep, not a data-flow check. It flags the rename
# half of the pattern (os.Rename with a temp-looking source) because that is
# the step that makes a write look atomic; a caller writing to a temp file and
# never renaming is not claiming atomicity and is not the risk here.

set -euo pipefail

cd "$(dirname "$0")/.."

# Files allowed to rename a temp file into place:
#   - internal/fsutil/fsutil.go: the canonical implementation every other
#     caller must route through.
#   - internal/fsutil/fsutil_test.go: exercises that implementation directly.
#   - scripts/check-atomic-writes.sh (this file): the pattern below is a grep
#     target, not a write path.
#   - internal/project/store.go: renames a temp *directory* (a finished bare
#     clone) into place. fsutil writes files; there is no file content to
#     stage here, so the helper does not apply.
#   - internal/confighot/watcher_test.go: deliberately simulates an editor's
#     write-tmp-then-rename save to exercise the config watcher. The rename is
#     the test's input, not a durability claim of its own.
ALLOWLIST=(
  "internal/fsutil/fsutil.go"
  "internal/fsutil/fsutil_test.go"
  "scripts/check-atomic-writes.sh"
  "internal/project/store.go"
  "internal/confighot/watcher_test.go"
)

is_allowlisted() {
  local f="$1"
  for allowed in "${ALLOWLIST[@]}"; do
    [[ "${f}" == "${allowed}" ]] && return 0
  done
  return 1
}

# os.Rename(<ident containing "tmp"/"temp">, ...) in any casing, which is how
# every hand-rolled copy in this repo spelled it.
PATTERN='os\.Rename\([^,)]*([tT]mp|[tT]emp)[^,)]*,'

files=()
while IFS= read -r -d '' file; do
  case "${file}" in
    *.go) files+=("${file}") ;;
  esac
done < <(git ls-files -z --cached --others --exclude-standard)

((${#files[@]} == 0)) && exit 0

fail=0
while IFS= read -r hit; do
  [[ -z "${hit}" ]] && continue
  path="${hit%%:*}"
  if is_allowlisted "${path}"; then
    continue
  fi
  if ((fail == 0)); then
    echo "error: hand-rolled atomic write outside internal/fsutil" >&2
    echo "Use fsutil.AtomicWrite (inherits the target's mode) or" >&2
    echo "fsutil.AtomicWriteMode (explicit mode) instead — they fsync the" >&2
    echo "parent directory, which every hand-rolled copy here forgot." >&2
    echo >&2
  fi
  fail=1
  echo "  ${hit}" >&2
done < <(grep -nHE "${PATTERN}" "${files[@]}" | sed 's#^\./##' || true)

if ((fail != 0)); then
  exit 1
fi

echo "check-atomic-writes: no hand-rolled temp-rename writes outside internal/fsutil"
