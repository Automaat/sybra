#!/usr/bin/env bash
# Every Wails-bound method that is also allowlisted for HTTP dispatch in
# internal/sybra/services.go (ServiceRegistry) must have a matching shim
# export in both frontend/src/lib/api.ts (the pick() re-export used by the
# rest of the frontend) and frontend/src/lib/api-http.ts (the actual HTTP
# implementation used by the web build). Miss either one and the method is
# silently unreachable from one of the two build targets.
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

# Exported Go method names allowlisted for HTTP dispatch, e.g. `"GetTask",`.
# Restricted to capitalized identifiers so this doesn't also match unrelated
# quoted strings (import paths, package names) elsewhere in the file. Uses
# BRE grep + sed instead of PCRE (-P) so this also runs on macOS/BSD grep.
read_lines_into methods < <(grep -oE '^[[:space:]]*"[A-Z][A-Za-z0-9_]*",?$' "${SERVICES_GO}" | sed -E 's/^[[:space:]]*"([A-Za-z0-9_]+)",?$/\1/' | sort -u)

if [[ "${#methods[@]}" -eq 0 ]]; then
  echo "::error::no methods parsed from ${SERVICES_GO} — check the extraction regex" >&2
  exit 1
fi

read_lines_into api_ts_exports < <(grep -oE '^export const [A-Za-z0-9_]+' "${API_TS}" | sed -E 's/^export const //' | sort -u)
read_lines_into api_http_exports < <(grep -oE '^export (async )?function [A-Za-z0-9_]+' "${API_HTTP_TS}" | sed -E 's/^export (async )?function //' | sort -u)

fail=0

for method in "${methods[@]}"; do
  if ! printf '%s\n' "${api_ts_exports[@]}" | grep -qx "${method}"; then
    echo "::error file=${API_TS}::HTTP-allowlisted method ${method} (${SERVICES_GO}) has no 'export const ${method}' shim" >&2
    fail=1
  fi
  if ! printf '%s\n' "${api_http_exports[@]}" | grep -qx "${method}"; then
    echo "::error file=${API_HTTP_TS}::HTTP-allowlisted method ${method} (${SERVICES_GO}) has no 'export function ${method}' implementation" >&2
    fail=1
  fi
done

if [[ "${fail}" -eq 0 ]]; then
  echo "api shim sync OK — ${#methods[@]} HTTP-allowlisted methods all have api.ts + api-http.ts shims"
fi

exit "${fail}"
