#!/usr/bin/env bash
# Proves check-atomic-writes.sh still fails on a reintroduced hand-rolled write.
# A drift gate that silently stops matching is worse than no gate: it reports
# green while the thing it guards rots.

set -euo pipefail

cd "$(dirname "$0")/.."

GATE="scripts/check-atomic-writes.sh"
PROBE="internal/fsutil/zz_atomic_write_probe.go"

cleanup() {
  rm -f "${PROBE}"
}
trap cleanup EXIT

# The gate scans tracked and untracked files, so an untracked probe is enough.
cat >"${PROBE}" <<'PROBE_EOF'
//go:build ignore

package fsutil

import "os"

func zzProbeHandRolledWrite(tmpName, path string) error {
	return os.Rename(tmpName, path)
}
PROBE_EOF

if bash "${GATE}" >/dev/null 2>&1; then
  echo "error: ${GATE} passed with a hand-rolled temp-rename write present" >&2
  echo "The gate has stopped matching the pattern it exists to catch." >&2
  exit 1
fi

cleanup
trap - EXIT

if ! bash "${GATE}" >/dev/null 2>&1; then
  echo "error: ${GATE} fails on the clean tree" >&2
  bash "${GATE}" >&2 || true
  exit 1
fi

echo "check-atomic-writes.test: gate catches drift and passes clean"
