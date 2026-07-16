#!/usr/bin/env bash
# Ban new code that resolves the bot's own GitHub identity via `gh api user`
# (or the REST /user endpoint) outside internal/github's single resolution
# point (viewerLoginE).
#
# /user is a user-to-server endpoint that ALWAYS 403s for GitHub App
# installation tokens, even when they are fully functional for everything
# else. The server authenticates as a GitHub App, so any /user-based identity
# lookup silently resolves to "" there.
#
# This has now bitten twice:
#   - #2032: Authenticated() preflighted with `gh api user` and false-negatived
#     on exactly the credential type it existed to support. Fixed by probing
#     /rate_limit instead.
#   - #2164: viewerLogin() resolved the review-attribution identity with
#     `gh api user`, returned "", froze review_phase at needs-approval, and
#     drove 112 duplicate reviews onto an external contributor's PR over 23h.
#
# Both times the fix was applied to one call site while its twin kept the bug.
# This check exists so the third occurrence fails in CI instead of on someone
# else's pull request.
#
# This is a literal-string grep, not an AST/data-flow check: it flags source
# that spells out the /user identity lookup. It cannot prove a given lookup is
# actually used for identity, so it narrows the risk rather than closing it.

set -euo pipefail

cd "$(dirname "$0")/.."

# Files allowed to reference the /user endpoint:
#   - internal/github/client.go: viewerLoginE(), the canonical resolution
#     point. It calls /user ONLY when App auth is disabled, where /user is
#     correct; under App auth it resolves <slug>[bot] from GET /app instead.
#   - internal/github/appauth.go: the comment on Authenticated() documenting
#     why /user must not be used (the #2032 fix).
#   - internal/github/viewer_login_test.go: asserts both branches, including
#     that /user is NOT called under App auth.
#   - internal/github/appauth_test.go: prose in the #2032 regression test
#     naming the banned call, not a lookup.
#   - scripts/check-no-viewer-user-endpoint.sh (this file): the patterns
#     below are grep targets, not a lookup.
ALLOWLIST=(
  "internal/github/client.go"
  "internal/github/appauth.go"
  "internal/github/viewer_login_test.go"
  "internal/github/appauth_test.go"
  "scripts/check-no-viewer-user-endpoint.sh"
)

is_allowlisted() {
  local f="$1"
  for allowed in "${ALLOWLIST[@]}"; do
    [[ "${f}" == "${allowed}" ]] && return 0
  done
  return 1
}

fail=0

# Matches the identity lookup in the shapes it actually appears in:
#   gh api user            -> e.run("api", "user", ...)
#   gh api /user           -> exec.Command("gh", "api", "/user")
#   "api", "user"          -> arg-slice form
PATTERN='"api",[[:space:]]*"/?user"|gh[[:space:]]+api[[:space:]]+/?user\b|"/user"'

files=()
while IFS= read -r -d '' file; do
  case "${file}" in
    *.go | *.sh | *.ts)
      files+=("${file}")
      ;;
  esac
done < <(git ls-files -z --cached --others --exclude-standard)

if ((${#files[@]} > 0)); then
  while IFS= read -r hit; do
    [[ -z "${hit}" ]] && continue
    f="${hit%%:*}"
    if is_allowlisted "${f}"; then
      continue
    fi
    if ((fail == 0)); then
      echo "ERROR: /user identity lookup outside internal/github's viewerLoginE()." >&2
      echo "/user always 403s for GitHub App installation tokens (#2032, #2164)." >&2
      echo "Call github.ViewerLogin()/viewerLoginE(), which is App-auth-aware, instead." >&2
      echo >&2
    fi
    fail=1
    echo "  ${hit}" >&2
  done < <(grep -nHE "${PATTERN}" "${files[@]}" 2>/dev/null | sed 's#^\./##' || true)
fi

if ((fail != 0)); then
  exit 1
fi

echo "check-no-viewer-user-endpoint: ok"
