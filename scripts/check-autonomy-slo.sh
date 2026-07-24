#!/usr/bin/env bash
# Autonomy SLO gate (#2441/#8650725d). Runs the offline, deterministic SLO
# golden-fixture suite in internal/evaluation and publishes a compliance
# table. The fixtures are committed Go code (internal/evaluation/slo_test.go)
# rather than a live audit read — this gate reproduces without network
# access or a running Sybra instance, and fails the same way in CI as it
# does locally.
#
# Exit non-zero on any fixture regression (a "before" fixture that stops
# breaching, or an "after" fixture that stops passing) or on any SLO test
# failure in the package.

set -euo pipefail

cd "$(dirname "$0")/.."

OUT="$(mktemp)"
trap 'rm -f "${OUT}"' EXIT

status=0
if ! go test ./internal/evaluation/... -run '^TestSLO|^TestEvaluateSLOs|^TestReconcileReports|^TestScanIdenticalRetryCap|^TestScanRestartCadence|^TestDefaultSLOTargets' -v > "${OUT}" 2>&1; then
  status=1
fi

cat "${OUT}"

TABLE="$(grep -E '^\s+slo_test\.go:[0-9]+: slo: ' "${OUT}" | sed -E 's/^\s+slo_test\.go:[0-9]+: slo: //' || true)"

if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  {
    echo "## Autonomy SLO gate"
    echo
    if [[ "${status}" -eq 0 ]]; then
      echo "All golden fixtures compliant after their fix; all breach before it."
    else
      echo "**FAILED** — see the job log for the failing fixture(s)."
    fi
    echo
    echo '```'
    if [[ -n "${TABLE}" ]]; then
      echo "${TABLE}"
    else
      echo "(no fixture rows captured — run with -v locally to inspect)"
    fi
    echo '```'
  } >> "${GITHUB_STEP_SUMMARY}"
fi

exit "${status}"
