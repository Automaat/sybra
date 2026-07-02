#!/usr/bin/env bash
# Fixture-based tests for generate-release-notes.sh.
#
# Each case creates a throwaway git repo with a scripted commit history and
# asserts the generated release notes body. Failures are recorded in
# FAIL_FLAG since each case runs in a subshell.

set -euo pipefail

cd "$(dirname "$0")/.."
SCRIPT="$(pwd)/scripts/generate-release-notes.sh"

FAIL_FLAG=$(mktemp)
trap 'rm -f "${FAIL_FLAG}"' EXIT

assert_contains() {
  local label="$1" haystack="$2" needle="$3"
  if [[ "${haystack}" != *"${needle}"* ]]; then
    echo "FAIL: ${label}: expected output to contain '${needle}'" >&2
    echo "--- got ---" >&2
    echo "${haystack}" >&2
    echo 1 >>"${FAIL_FLAG}"
  else
    echo "ok: ${label}"
  fi
}

assert_eq() {
  local label="$1" expected="$2" actual="$3"
  if [[ "${expected}" != "${actual}" ]]; then
    echo "FAIL: ${label}: expected '${expected}', got '${actual}'" >&2
    echo 1 >>"${FAIL_FLAG}"
  else
    echo "ok: ${label}"
  fi
}

init_repo() {
  git init -q
  git config user.email "test@example.com"
  git config user.name "Test"
}

commit() {
  git commit -q --allow-empty -m "$1"
}

# --- Case: no previous tag -> full history, conventional commits ---------
(
  dir=$(mktemp -d)
  cd "${dir}"
  init_repo
  commit "feat: add widget"
  commit "fix: crash on startup"

  OUT=$("${SCRIPT}")
  assert_contains "no-tag: has Features header" "${OUT}" "### Features"
  assert_contains "no-tag: has feat line" "${OUT}" "- feat: add widget"
  assert_contains "no-tag: has Bug Fixes header" "${OUT}" "### Bug Fixes"
  assert_contains "no-tag: has fix line" "${OUT}" "- fix: crash on startup"
  rm -rf "${dir}"
)

# --- Case: existing tag -> only commits after the tag are included -------
(
  dir=$(mktemp -d)
  cd "${dir}"
  init_repo
  commit "chore: init"
  git -c tag.gpgSign=false tag v1.0.0
  commit "feat: add gizmo"

  OUT=$("${SCRIPT}" "v1.0.0")
  assert_contains "existing-tag: has new feat" "${OUT}" "- feat: add gizmo"
  case "${OUT}" in
  *"chore: init"*)
    echo "FAIL: existing-tag: pre-tag commit leaked into notes" >&2
    echo 1 >>"${FAIL_FLAG}"
    ;;
  *) echo "ok: existing-tag: pre-tag commit excluded" ;;
  esac
  rm -rf "${dir}"
)

# --- Case: non-conventional commit subjects land under Other -------------
(
  dir=$(mktemp -d)
  cd "${dir}"
  init_repo
  commit "Bump dependency to 2.0"
  commit "wip stuff"

  OUT=$("${SCRIPT}")
  assert_contains "non-conventional: has Other header" "${OUT}" "### Other"
  assert_contains "non-conventional: has bump line" "${OUT}" "- Bump dependency to 2.0"
  assert_contains "non-conventional: has wip line" "${OUT}" "- wip stuff"
  rm -rf "${dir}"
)

# --- Case: no commits in range -> fallback message ------------------------
(
  dir=$(mktemp -d)
  cd "${dir}"
  init_repo
  commit "chore: init"
  git -c tag.gpgSign=false tag v1.0.0

  OUT=$("${SCRIPT}" "v1.0.0")
  assert_eq "empty-range: fallback message" "No changes recorded." "${OUT}"
  rm -rf "${dir}"
)

if [[ -s "${FAIL_FLAG}" ]]; then
  echo "generate-release-notes.test.sh: FAILED" >&2
  exit 1
fi
echo "generate-release-notes.test.sh: all tests passed"
