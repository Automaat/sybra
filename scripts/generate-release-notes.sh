#!/usr/bin/env bash
# Generates a conventional-commit-grouped release notes body from git log.
#
# Usage: generate-release-notes.sh [prev-version]
#
# Walks commit subjects between prev-version (exclusive) and HEAD in the
# git repo in the current working directory, bucketing them by conventional
# commit type. Commits that don't match a known type land under "### Other".
# Prints the resulting Markdown to stdout.

set -euo pipefail

PREV="${1:-}"

if [[ -z "${PREV}" ]]; then
  RANGE="HEAD"
else
  RANGE="${PREV}..HEAD"
fi

declare -A TITLES
TITLES[feat]="### Features"
TITLES[fix]="### Bug Fixes"
TITLES[perf]="### Performance"
TITLES[refactor]="### Refactoring"
TITLES[docs]="### Documentation"
TITLES[ci]="### CI"
TITLES[test]="### Tests"
TITLES[build]="### Build"
TITLES[chore]="### Chores"

declare -A LINES

TYPE_RE='^([a-z]+)[(:!]'
while IFS= read -r subject; do
  if [[ "${subject}" =~ ${TYPE_RE} ]]; then
    TYPE="${BASH_REMATCH[1]}"
  else
    TYPE=""
  fi
  if [[ -n "${TYPE}" && -n "${TITLES[${TYPE}]+x}" ]]; then
    LINES[${TYPE}]+="- ${subject}"$'\n'
  else
    LINES[other]+="- ${subject}"$'\n'
  fi
done < <(git log "${RANGE}" --format="%s" 2>/dev/null)

NOTES=""
for TYPE in feat fix perf refactor docs ci test build chore; do
  if [[ -n "${LINES[${TYPE}]:-}" ]]; then
    NOTES+="${TITLES[${TYPE}]}"$'\n'
    NOTES+="${LINES[${TYPE}]}"$'\n'
  fi
done
if [[ -n "${LINES[other]:-}" ]]; then
  NOTES+="### Other"$'\n'
  NOTES+="${LINES[other]}"$'\n'
fi

if [[ -z "${NOTES}" ]]; then
  NOTES="No changes recorded."
fi

printf '%s' "${NOTES}"
