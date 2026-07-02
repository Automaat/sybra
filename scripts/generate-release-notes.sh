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

title_for() {
  case "$1" in
    feat) echo "### Features" ;;
    fix) echo "### Bug Fixes" ;;
    perf) echo "### Performance" ;;
    refactor) echo "### Refactoring" ;;
    docs) echo "### Documentation" ;;
    ci) echo "### CI" ;;
    test) echo "### Tests" ;;
    build) echo "### Build" ;;
    chore) echo "### Chores" ;;
  esac
}

LINES_feat="" LINES_fix="" LINES_perf="" LINES_refactor="" LINES_docs=""
LINES_ci="" LINES_test="" LINES_build="" LINES_chore="" LINES_other=""

TYPE_RE='^([a-z]+)[(:!]'
SUBJECTS=$(git log "${RANGE}" --format="%s")
while IFS= read -r subject; do
  if [[ -z "${subject}" ]]; then
    continue
  fi
  if [[ "${subject}" =~ ${TYPE_RE} ]]; then
    TYPE="${BASH_REMATCH[1]}"
  else
    TYPE=""
  fi
  case "${TYPE}" in
    feat) LINES_feat+="- ${subject}"$'\n' ;;
    fix) LINES_fix+="- ${subject}"$'\n' ;;
    perf) LINES_perf+="- ${subject}"$'\n' ;;
    refactor) LINES_refactor+="- ${subject}"$'\n' ;;
    docs) LINES_docs+="- ${subject}"$'\n' ;;
    ci) LINES_ci+="- ${subject}"$'\n' ;;
    test) LINES_test+="- ${subject}"$'\n' ;;
    build) LINES_build+="- ${subject}"$'\n' ;;
    chore) LINES_chore+="- ${subject}"$'\n' ;;
    *) LINES_other+="- ${subject}"$'\n' ;;
  esac
done <<<"${SUBJECTS}"

NOTES=""
for TYPE in feat fix perf refactor docs ci test build chore; do
  VAR="LINES_${TYPE}"
  if [[ -n "${!VAR}" ]]; then
    NOTES+="$(title_for "${TYPE}")"$'\n'
    NOTES+="${!VAR}"$'\n'
  fi
done
if [[ -n "${LINES_other}" ]]; then
  NOTES+="### Other"$'\n'
  NOTES+="${LINES_other}"$'\n'
fi

if [[ -z "${NOTES}" ]]; then
  NOTES="No changes recorded."
fi

printf '%s' "${NOTES}"
