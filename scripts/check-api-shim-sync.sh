#!/usr/bin/env bash
# Every Wails-bound method that is also allowlisted for HTTP dispatch in
# internal/sybra/services.go (ServiceRegistry) must have a matching
# implementation in frontend/src/lib/api-http.ts, which is the frontend's only
# transport. Miss it and the method is unreachable from the UI.
#
# frontend/src/lib/api.ts is the import surface the rest of the frontend uses;
# it re-exports api-http.ts wholesale, and this script asserts that it still
# does — a hand-written export list there would reintroduce the second place a
# method has to be added.
#
# services.go is the source of truth: it is the one hand-maintained list a
# method must be added to for HTTP dispatch to work at all, so drift here
# means a shim genuinely wasn't written — not just a naming mismatch.

set -euo pipefail

cd "$(dirname "$0")/.."

SERVICES_GO="internal/sybra/services.go"
API_TS="frontend/src/lib/api.ts"
API_HTTP_TS="frontend/src/lib/api-http.ts"

for f in "${SERVICES_GO}" "${API_TS}" "${API_HTTP_TS}"; do
  if [[ ! -f "${f}" ]]; then
    echo "::error::${f} not found" >&2
    exit 1
  fi
done

# Reads newline-delimited stdin into the named array. Used instead of
# `mapfile -t` (bash 4+) so this also runs on the default macOS /bin/bash (3.2).
read_lines_into() {
  local __arr_name="$1"
  eval "${__arr_name}=()"
  local line
  while IFS= read -r line; do
    eval "${__arr_name}+=(\"\${line}\")"
  done
}

# Only Wails-bound services need frontend shims. An HTTP-only control-plane
# service (in the HTTP allowlist but with no wails3 binding, e.g. QueueService)
# has no api.ts pick() counterpart and no frontend caller, so it is exempt —
# gated below by whether its binding file exists. Portable awk for BSD + gawk;
# the capitalized-only method regex still skips import paths and package names.
BINDING_DIR="frontend/bindings/github.com/Automaat/sybra/internal/sybra"
read_lines_into methods < <(awk -v bdir="${BINDING_DIR}" '
  /httpapi\.NewService\(/ {
    svc = $0
    sub(/^[[:space:]]*"/, "", svc)
    sub(/".*/, "", svc)
    wails = (system("test -f \"" bdir "/" tolower(svc) ".ts\"") == 0)
    next
  }
  /^[[:space:]]*"[A-Z][A-Za-z0-9_]*",?[[:space:]]*$/ {
    if (wails) { m = $0; sub(/^[[:space:]]*"/, "", m); sub(/",?[[:space:]]*$/, "", m); print m }
  }
' "${SERVICES_GO}" | sort -u)

if [[ "${#methods[@]}" -eq 0 ]]; then
  echo "::error::no methods parsed from ${SERVICES_GO} — check the extraction regex" >&2
  exit 1
fi

if ! grep -qE "^export \* from './api-http\.js'" "${API_TS}"; then
  echo "::error file=${API_TS}::must re-export api-http.ts wholesale (export * from './api-http.js')" >&2
  exit 1
fi

# The generated service bindings call Wails IPC, which nothing serves any more:
# a component importing one posts to /wails/runtime and gets 405. Only the
# models.* files (plain type/class declarations) may be imported from src.
stray_bindings="$(grep -rn "bindings/" frontend/src \
  --include='*.ts' --include='*.svelte' \
  | grep -v '/models\.js' \
  | grep -v '^frontend/src/lib/api\.ts:' || true)"
if [[ -n "${stray_bindings}" ]]; then
  echo "::error::frontend/src must reach the server through \$lib/api; these import a generated Wails call path, which nothing serves:" >&2
  echo "${stray_bindings}" >&2
  exit 1
fi

read_lines_into api_http_exports < <(grep -oE '^export (async )?function [A-Za-z0-9_]+' "${API_HTTP_TS}" | sed -E 's/^export (async )?function //' | sort -u)

fail=0

for method in "${methods[@]}"; do
  if ! printf '%s\n' "${api_http_exports[@]}" | grep -qx "${method}"; then
    echo "::error file=${API_HTTP_TS}::HTTP-allowlisted method ${method} (${SERVICES_GO}) has no 'export function ${method}' implementation" >&2
    fail=1
  fi
done

if [[ "${fail}" -eq 0 ]]; then
  echo "api shim sync OK — ${#methods[@]} HTTP-allowlisted methods all have api-http.ts implementations"
fi

exit "${fail}"
