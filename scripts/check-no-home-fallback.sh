#!/usr/bin/env bash
# Ban new code that resolves the operator's real Sybra home (~/.sybra) as a
# silent fallback outside internal/config's single resolution point
# (config.HomeDir()). The 2026-07-06 board wipe (#1576) happened because a
# test suite defaulted to `~/.sybra` when SYBRA_HOME was unset and then
# deleted files there. Any code path that reconstructs "$HOME + .sybra" is
# the same trap waiting to be reintroduced.
#
# This is a literal-string grep, not an AST/data-flow check — it flags any
# string literal that spells out the ".sybra" suffix (bare, concatenated, or
# inside a format string), since that's the actual pattern that caused the
# incident. It intentionally does not flag every os.UserHomeDir()/homedir()
# call (e.g. resolving ~/.claude, ~/.codex, or generic "~/" expansion), only
# literals that also spell out ".sybra" — it does not verify the literal is
# actually combined with a home-dir lookup, so it can only narrow the risk,
# not close it.

set -euo pipefail

cd "$(dirname "$0")/.."

# Files allowed to hold the operator-home + ".sybra" pattern:
#   - internal/config/config_defaults.go: the canonical resolution point
#     (config.HomeDir()) all other code must call into instead.
#   - internal/config/config_test.go: tests config.HomeDir()'s own fallback.
#   - frontend/e2e/lib/sybra-home.ts: computes the real home path only to
#     REJECT it (isolatedSybraHome fails closed if SYBRA_HOME resolves there)
#     — it is the fix for #1576, not a reintroduction of the bug.
#   - scripts/sybra-dev-supervisor.sh: launches the operator's own real dev
#     instance and intentionally mirrors config.HomeDir()'s exact fallback
#     formula, since a shell script cannot import the Go resolver.
#   - scripts/check-no-home-fallback.sh (this file): the pattern strings
#     below are grep targets, not a fallback resolution.
#   - cmd/gen-config-docs/main.go: normalizes config.HomeDir()'s own output
#     back to the display string "~/.sybra" for docs/CONFIG.md, so the
#     generated doc is stable across machines/users — it consumes
#     config.HomeDir(), it doesn't reconstruct a fallback to it.
#   - internal/config/config_docs_generated_test.go: comment referencing
#     "~/.sybra" in prose, not a home-fallback code path.
ALLOWLIST=(
  "internal/config/config_defaults.go"
  "internal/config/config_test.go"
  "frontend/e2e/lib/sybra-home.ts"
  "scripts/sybra-dev-supervisor.sh"
  "cmd/gen-config-docs/main.go"
  "internal/config/config_docs_generated_test.go"
  "scripts/check-no-home-fallback.sh"
)

is_allowlisted() {
  local f="$1"
  for allowed in "${ALLOWLIST[@]}"; do
    [[ "${f}" == "${allowed}" ]] && return 0
  done
  return 1
}

fail=0

# Go: filepath.Join(<home-ish>, ".sybra"), bare string literal ".sybra", or
# a concatenation/format-string literal ending in ".sybra" (e.g.
# `home + "/.sybra"`, `fmt.Sprintf("%s/.sybra", home)`, `"~/.sybra"`,
# `home + `+"/.sybra"`` as a raw (backtick) string). Matches any
# double-quoted OR backtick-quoted literal whose content ends in ".sybra"
# immediately before the closing delimiter — deliberately does NOT match
# ".sybra/<subpath>" literals, since those are overwhelmingly descriptive
# paths (log/doc text, comments) rather than a home-fallback reconstruction,
# and matching them exploded into hundreds of unrelated false positives when
# tried.
while IFS=: read -r file line _; do
  [[ -z "${file}" ]] && continue
  if ! is_allowlisted "${file}"; then
    echo "::error file=${file},line=${line}::found a Go home+\".sybra\" fallback outside internal/config.HomeDir() (see #1576). Call config.HomeDir() instead of reconstructing it." >&2
    fail=1
  fi
done < <(grep -rnE '\.sybra["`]' --include="*.go" . | grep -v '/vendor/' | sed 's#^\./##')

# TS/JS/Svelte: join(homedir(), '.sybra') or a concatenation/template literal
# ending in '.sybra' (e.g. `${home}/.sybra`, `home + "/.sybra"`).
while IFS=: read -r file line _; do
  [[ -z "${file}" ]] && continue
  if ! is_allowlisted "${file}"; then
    echo "::error file=${file},line=${line}::found a TS home+'.sybra' fallback outside internal/config.HomeDir() (see #1576). Read SYBRA_HOME explicitly instead of defaulting to the operator's real home." >&2
    fail=1
  fi
done < <(grep -rnE "\.sybra['\"\`]" --include="*.ts" --include="*.js" --include="*.svelte" . | grep -v '/node_modules/' | grep -v '/bindings/' | grep -v '/dist/' | grep -v '/dist-web/' | sed 's#^\./##')

# Shell: $HOME/.sybra or ${HOME}/.sybra fallback defaults.
while IFS=: read -r file line _; do
  [[ -z "${file}" ]] && continue
  if ! is_allowlisted "${file}"; then
    echo "::error file=${file},line=${line}::found a shell HOME+.sybra fallback outside internal/config.HomeDir() (see #1576). Require SYBRA_HOME explicitly (e.g. ': \"\${SYBRA_HOME:?set SYBRA_HOME}\"') instead of defaulting to the operator's real home." >&2
    fail=1
  fi
done < <(grep -rnE '\$\{?HOME\}?/\.sybra' --include="*.sh" . | sed 's#^\./##')

if [[ "${fail}" -eq 0 ]]; then
  echo "no-home-fallback OK — no operator-home .sybra fallbacks found outside the allowlist"
fi

exit "${fail}"
