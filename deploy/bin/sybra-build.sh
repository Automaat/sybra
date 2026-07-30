#!/usr/bin/env bash
# ExecStartPre for the sybra systemd unit. Builds the current $SYBRA_SRC_DIR
# checkout into a fresh, versioned candidate directory, preflights it against
# the exact live config, and only then atomically repoints the "current"
# release pointer. A candidate that fails is quarantined by source-sha +
# live-config fingerprint so a subsequent identical deploy attempt (e.g. the
# next autoupdate poll) is rejected up front. Deterministic phases (config
# preflight) are quarantined on the first failure; transient build phases
# (mise install, npm ci, go build, sandbox smoke) get a retry budget and are
# only quarantined after SYBRA_BUILD_RETRY_THRESHOLD consecutive failures, so a
# one-off registry/network/toolchain flake cannot pin the host to the old
# release forever — see write_quarantine in sybra-deploy-lib.sh. Runtime health
# (does the activated release actually start and answer /health) is a
# separate, later concern handled by sybra-healthcheck.sh (ExecStartPost).
set -uo pipefail
shopt -s nullglob

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LOG_TAG=sybra-build
# shellcheck source=./sybra-deploy-lib.sh
source "$SCRIPT_DIR/sybra-deploy-lib.sh"

CLI_LINK_DIR="${HOME:-/home/sybra}/.local/bin"

have_last_good_build() {
  local target
  target="$(resolved_target "$LAST_GOOD_LINK")"
  [[ -n "$target" && -x "$target/sybra-server" && -f "$target/web/index.html" ]]
}

keep_last_good_or_fail() {
  log "build failed: $1"
  if have_last_good_build; then
    log "keeping last-good release $(resolved_target "$LAST_GOOD_LINK"); service will start on it"
    exit 0
  fi
  log "no prior good release to fall back on; failing the unit"
  exit 1
}

# Retry budget for transient build phases before they are quarantined.
BUILD_RETRY_THRESHOLD="${SYBRA_BUILD_RETRY_THRESHOLD:-3}"

# is_deterministic_phase reports whether a phase's outcome is fully determined
# by the source sha + live config, so retrying it for unchanged inputs is
# pointless and it should be quarantined on the first failure. Every other
# phase depends on transient host/network/toolchain state.
is_deterministic_phase() {
  case "$1" in
    config-preflight) return 0 ;;
    *) return 1 ;;
  esac
}

# reject_candidate discards the candidate then decides whether to quarantine
# the sha+config combo (so the next deploy attempt short-circuits before doing
# any real work) or to leave it retriable. Deterministic phases quarantine
# immediately; transient build phases only quarantine after
# BUILD_RETRY_THRESHOLD consecutive failures, so a one-off flake does not pin
# the host to the old release forever. Either way it defers to
# keep_last_good_or_fail for the exit-code/fallback contract.
reject_candidate() {
  local phase="$1" message="$2"
  rm -rf "$CANDIDATE_DIR"
  if is_deterministic_phase "$phase"; then
    write_quarantine "$KEY" "$phase" "$message" "$SHA"
    keep_last_good_or_fail "$phase: $message"
  fi
  local count
  count="$(record_build_failure "$KEY")"
  if (( count >= BUILD_RETRY_THRESHOLD )); then
    write_quarantine "$KEY" "$phase" "$message (failed ${count}x)" "$SHA"
    keep_last_good_or_fail "$phase: $message"
  fi
  log "build phase $phase failed (attempt $count/$BUILD_RETRY_THRESHOLD); leaving retriable — next deploy will rebuild rather than quarantine"
  keep_last_good_or_fail "$phase: $message"
}

prune_releases() {
  local keep_n=3
  [[ -d "$RELEASES_DIR" ]] || return 0
  local cur last
  cur="$(resolved_target "$CURRENT_LINK")"
  last="$(resolved_target "$LAST_GOOD_LINK")"
  local -A keep=()
  [[ -n "$cur" ]] && keep["$cur"]=1
  [[ -n "$last" ]] && keep["$last"]=1
  local recent d
  mapfile -t recent < <(find "$RELEASES_DIR" -maxdepth 1 -mindepth 1 -type d ! -name '*.building' -printf '%T@ %p\n' 2>/dev/null | sort -rn | head -n "$keep_n" | awk '{print $2}')
  for d in "${recent[@]}"; do keep["$d"]=1; done
  for d in "$RELEASES_DIR"/*/; do
    d="${d%/}"
    [[ "$d" == *.building ]] && continue
    [[ -n "${keep[$d]:-}" ]] && continue
    log "pruning old release $d"
    rm -rf "$d"
  done
}

ensure_cli_symlink() {
  # Points at $CURRENT_LINK, not a specific release, so it always resolves to
  # whichever release is active — including after a healthcheck-driven
  # rollback — without needing to be re-linked on every build.
  if ! (mkdir -p "$CLI_LINK_DIR" && ln -sf "$CURRENT_LINK/sybra-cli" "$CLI_LINK_DIR/sybra-cli"); then
    log "warning: failed to symlink sybra-cli into $CLI_LINK_DIR; binary is still at $CURRENT_LINK/sybra-cli"
  fi
}

activate_candidate() {
  local final_dir="$RELEASES_DIR/$ID"
  rm -rf "$final_dir"
  mv -T "$CANDIDATE_DIR" "$final_dir"
  atomic_symlink "$final_dir" "$CURRENT_LINK"
  clear_quarantine "$KEY"
  clear_build_failures "$KEY"
  log "activated release $ID -> $CURRENT_LINK"
}

main() {
  [[ -d "$SRC" ]] || keep_last_good_or_fail "source dir $SRC missing"

  SHA="$(candidate_sha)"
  KEY="$(candidate_key "$SHA")"

  if is_quarantined "$KEY"; then
    log "candidate sha=$SHA config=$(config_fingerprint) is quarantined ($(head -n1 "$(quarantine_file "$KEY")" 2>/dev/null | tr '\n' ' ')); see $(quarantine_file "$KEY"). Skipping rebuild — change source or live config to retry."
    if have_last_good_build; then
      log "staying on last-good release $(resolved_target "$LAST_GOOD_LINK")"
      exit 0
    fi
    log "no prior good release available while candidate is quarantined; failing the unit"
    exit 1
  fi

  command -v mise >/dev/null 2>&1 || keep_last_good_or_fail "mise not on PATH"

  ID="${SHA}-$(date +%s)"
  CANDIDATE_DIR="$RELEASES_DIR/$ID.building"
  rm -rf "$CANDIDATE_DIR"
  mkdir -p "$CANDIDATE_DIR"

  cd "$SRC" || keep_last_good_or_fail "source dir $SRC missing"

  log "building candidate sha=$SHA id=$ID"

  mise install >"$(detail_log_path "$ID" mise-install)" 2>&1 \
    || reject_candidate "mise-install" "mise install failed"

  ( cd frontend && mise exec -- npm ci --no-audit --no-fund && mise exec -- npm run build:web ) \
      >"$(detail_log_path "$ID" frontend-build)" 2>&1 \
    || reject_candidate "frontend-build" "frontend build failed"

  CGO_ENABLED=0 mise exec -- go build -trimpath -o "$CANDIDATE_DIR/sybra-server" ./cmd/sybra-server \
      >"$(detail_log_path "$ID" go-build-server)" 2>&1 \
    || reject_candidate "go-build-server" "go build ./cmd/sybra-server failed"

  # Built alongside sybra-server, from the same source checkout, in the same
  # candidate directory swapped in atomically below — this is what keeps the
  # CLI's config-schema handling from drifting out of sync with the running
  # server (#2619).
  CGO_ENABLED=0 mise exec -- go build -trimpath -o "$CANDIDATE_DIR/sybra-cli" ./cmd/sybra-cli \
      >"$(detail_log_path "$ID" go-build-cli)" 2>&1 \
    || reject_candidate "go-build-cli" "go build ./cmd/sybra-cli failed"

  if command -v bwrap >/dev/null 2>&1; then
    log "running linked-worktree sandbox smoke"
    mise exec -- go test ./internal/agent -run '^TestSandboxEnforce_LinkedWorktreeGitOps$' -count=1 \
        >"$(detail_log_path "$ID" sandbox-smoke)" 2>&1 \
      || reject_candidate "sandbox-smoke" "linux sandbox git smoke test failed"
  fi

  cp -a frontend/dist-web "$CANDIDATE_DIR/web" \
    || reject_candidate "stage-web" "failed to stage frontend/dist-web into candidate"

  # Preflight: run the candidate binary's own config validation against the
  # exact live config before it is ever activated. Deterministic given an
  # unchanged sha+config, so a failure here is quarantined immediately rather
  # than retried — see the acceptance bar in issue #2729.
  log "preflight: validating live config (fingerprint=$(config_fingerprint)) against candidate binary"
  "$CANDIDATE_DIR/sybra-server" -check-config >"$(detail_log_path "$ID" config-preflight)" 2>&1 \
    || reject_candidate "config-preflight" "candidate binary rejected the live config; see $(detail_log_path "$ID" config-preflight) on this host for detail"

  activate_candidate
  prune_releases
  ensure_cli_symlink

  log "build complete: activated sha=$SHA id=$ID"
}

with_deploy_lock main
rc=$?
if [[ $rc -eq 2 ]]; then
  # Lock contention means another deploy is already driving current/last-good
  # to a consistent state; back off quietly rather than failing the unit.
  exit 0
fi
exit "$rc"
