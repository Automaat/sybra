#!/usr/bin/env bash
# Fixture-based tests for resolve-release-version.sh.
#
# Each case creates a throwaway git repo, tags it as needed, runs the
# script against it, and asserts the emitted version=/prev_version= pairs.
# Failures are recorded in FAIL_FLAG since each case runs in a subshell.

set -euo pipefail

cd "$(dirname "$0")/.."
SCRIPT="$(pwd)/scripts/resolve-release-version.sh"

FAIL_FLAG=$(mktemp)
trap 'rm -f "${FAIL_FLAG}"' EXIT

assert_eq() {
  local label="$1" expected="$2" actual="$3"
  if [[ "${expected}" != "${actual}" ]]; then
    echo "FAIL: ${label}: expected '${expected}', got '${actual}'" >&2
    echo 1 >>"${FAIL_FLAG}"
  else
    echo "ok: ${label}"
  fi
}

extract_output_value() {
  local key="$1" content="$2"
  sed -n "s/^${key}=//p" <<<"${content}" | head -n1
}

init_repo() {
  git init -q
  git config user.email "test@example.com"
  git config user.name "Test"
  git commit -q --allow-empty -m "chore: init"
}

# --- Case: no existing tag, no requested version -> v0.1.0 ---------------
(
  dir=$(mktemp -d)
  cd "${dir}"
  init_repo

  OUT=$("${SCRIPT}")
  VERSION=$(extract_output_value "version" "${OUT}")
  PREV=$(extract_output_value "prev_version" "${OUT}")
  assert_eq "no-tag: version" "v0.1.0" "${VERSION}"
  assert_eq "no-tag: prev_version" "" "${PREV}"
  rm -rf "${dir}"
)

# --- Case: existing tag, no requested version -> minor bump --------------
(
  dir=$(mktemp -d)
  cd "${dir}"
  init_repo
  git -c tag.gpgSign=false tag v1.4.0

  OUT=$("${SCRIPT}")
  VERSION=$(extract_output_value "version" "${OUT}")
  PREV=$(extract_output_value "prev_version" "${OUT}")
  assert_eq "existing-tag: version" "v1.5.0" "${VERSION}"
  assert_eq "existing-tag: prev_version" "v1.4.0" "${PREV}"
  rm -rf "${dir}"
)

# --- Case: explicit requested version overrides auto-bump ----------------
(
  dir=$(mktemp -d)
  cd "${dir}"
  init_repo
  git -c tag.gpgSign=false tag v1.4.0

  OUT=$("${SCRIPT}" "v2.0.0")
  VERSION=$(extract_output_value "version" "${OUT}")
  PREV=$(extract_output_value "prev_version" "${OUT}")
  assert_eq "requested-version: version" "v2.0.0" "${VERSION}"
  assert_eq "requested-version: prev_version" "v1.4.0" "${PREV}"
  rm -rf "${dir}"
)

# --- Case: invalid requested version fails validation ----------------------
(
  dir=$(mktemp -d)
  cd "${dir}"
  init_repo

  if "${SCRIPT}" $'v2.0.0\nextra=bad' >/dev/null 2>&1; then
    echo "FAIL: invalid-requested-version: expected script to reject newline input" >&2
    echo 1 >>"${FAIL_FLAG}"
  else
    echo "ok: invalid-requested-version: rejected malformed input"
  fi
  rm -rf "${dir}"
)

# --- Case: GITHUB_OUTPUT is populated when set ----------------------------
(
  dir=$(mktemp -d)
  out_file=$(mktemp)
  cd "${dir}"
  init_repo

  GITHUB_OUTPUT="${out_file}" "${SCRIPT}" >/dev/null
  CONTENTS=$(cat "${out_file}")
  case "${CONTENTS}" in
  *"version=v0.1.0"*) echo "ok: github-output: version written" ;;
  *)
    echo "FAIL: github-output: version= missing from GITHUB_OUTPUT (got: ${CONTENTS})" >&2
    echo 1 >>"${FAIL_FLAG}"
    ;;
  esac
  rm -rf "${dir}" "${out_file}"
)

if [[ -s "${FAIL_FLAG}" ]]; then
  echo "resolve-release-version.test.sh: FAILED" >&2
  exit 1
fi
echo "resolve-release-version.test.sh: all tests passed"
