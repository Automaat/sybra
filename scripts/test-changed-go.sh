#!/usr/bin/env bash
# Fast local regression loop. CI still runs the whole tree with race/e2e/DB
# coverage; this selects surviving Go packages touched since the trusted base.
set -euo pipefail
base="${SYBRA_CHECK_BASE_REF:-origin/main}"
base_sha="$(git rev-parse --verify "$base^{commit}")"
merge_base="$(git merge-base "$base_sha" HEAD)"
# Compare against the working tree too, so this is useful before a local
# commit. Capture git's status explicitly (process substitution hides it).
changed_files="$(mktemp "${TMPDIR:-/tmp}/sybra-changed-go.XXXXXX")"
trap 'rm -f "$changed_files"' EXIT
git diff --name-only -z --no-renames --diff-filter=ACMRD "$merge_base" -- '*.go' > "$changed_files"
git ls-files --others --exclude-standard -z -- '*.go' >> "$changed_files"
packages=()
while IFS= read -r -d '' path; do
  [[ "$path" == *.go ]] || continue
  package="./$(dirname "$path")"
  [[ -d "$package" ]] || continue
  # A removed last Go source leaves no package to test.
  compgen -G "$package/*.go" >/dev/null || continue
  seen=false
  for existing in "${packages[@]+${packages[@]}}"; do
    [[ "$existing" == "$package" ]] && seen=true
  done
  [[ "$seen" == true ]] || packages+=("$package")
done < "$changed_files"
if (( ${#packages[@]} == 0 )); then
  echo "No changed Go packages; full regression coverage is owned by CI."
  exit 0
fi
go test -timeout 5m "${packages[@]}"
