#!/usr/bin/env bash
# Resolves the release version to publish and the previous release tag.
#
# Usage: resolve-release-version.sh [requested-version]
#
# If a version is given (e.g. from a workflow_dispatch input) it is used
# as-is. Otherwise the latest vX.Y.Z tag in the current repo is auto-bumped
# to the next minor (or v0.1.0 if no tag exists yet).
#
# Always prints `version=` and `prev_version=` as key=value lines to stdout.
# When $GITHUB_OUTPUT is set, the same lines are also appended there.
#
# Reads tags from the git repo in the current working directory.

set -euo pipefail

REQUESTED_VERSION="${1:-}"

LATEST=$(git tag --list 'v*.*.*' --sort=-v:refname | head -n1)

if [[ -n "${REQUESTED_VERSION}" ]]; then
  VERSION="${REQUESTED_VERSION}"
elif [[ -z "${LATEST}" ]]; then
  VERSION="v0.1.0"
else
  MAJOR=$(echo "${LATEST}" | cut -d. -f1)
  MINOR=$(echo "${LATEST}" | cut -d. -f2)
  VERSION="${MAJOR}.$((MINOR + 1)).0"
fi

echo "Publishing version: ${VERSION} (prev: ${LATEST})" >&2

OUTPUT="version=${VERSION}
prev_version=${LATEST}"

echo "${OUTPUT}"

if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  echo "${OUTPUT}" >>"${GITHUB_OUTPUT}"
fi
