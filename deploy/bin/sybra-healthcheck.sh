#!/usr/bin/env bash
# ExecStartPost for the sybra systemd unit. Polls the just-started release's
# /health endpoint; on success promotes it to "last-good" and clears any
# health-failure counter for this candidate. On failure it rolls the
# "current" pointer back to the last known-good release (if any) and records
# a failure against the candidate's quarantine key — once the failure count
# crosses the threshold the candidate is quarantined outright, so the next
# restart's ExecStartPre (sybra-build.sh) no longer rebuilds/reactivates it
# (it will already find "current" == "last-good"). Exiting non-zero here
# fails the unit's start, which is what drives the actual restart via
# Restart=on-failure + RestartSec — this script never calls systemctl itself.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LOG_TAG=sybra-healthcheck
# shellcheck source=./sybra-deploy-lib.sh
source "$SCRIPT_DIR/sybra-deploy-lib.sh"

# health_url resolves the URL to poll. SYBRA_SERVER_TARGET takes either a bare
# host and port or a full origin — the same two forms sybra-cli and the desktop
# app accept — so a board that terminates TLS names an https origin here.
# Prefixing http:// unconditionally produced http://https://host:port/health,
# which connects to nothing: the start counted as unhealthy and, on a first
# install with no last-good release to roll back to, the unit failed and
# rebuilt from source on every restart.
health_url() {
  if [[ -n "${SYBRA_HEALTH_URL:-}" ]]; then
    printf '%s\n' "$SYBRA_HEALTH_URL"
    return 0
  fi
  local target="${SYBRA_SERVER_TARGET:-127.0.0.1:8080}"
  target="${target%/}"
  case "$target" in
    http://*/* | https://*/*)
      log "warning: SYBRA_SERVER_TARGET=$target carries a path; health check uses its origin only" >&2
      local scheme="${target%%://*}" rest="${target#*://}"
      printf '%s://%s/health\n' "$scheme" "${rest%%/*}"
      ;;
    http://* | https://*) printf '%s/health\n' "$target" ;;
    *) printf 'http://%s/health\n' "$target" ;;
  esac
}

HEALTH_URL="$(health_url)"

# A board that terminates TLS with its own certificate is reachable only if
# curl is told about it, the same way an agent's CLI is (SYBRA_SERVER_CA).
CURL_TLS_ARGS=()
if [[ -n "${SYBRA_SERVER_CA:-}" ]]; then
  CURL_TLS_ARGS+=(--cacert "$SYBRA_SERVER_CA")
fi
TIMEOUT_SEC="${SYBRA_HEALTH_TIMEOUT_SEC:-60}"
INTERVAL_SEC="${SYBRA_HEALTH_INTERVAL_SEC:-2}"
QUARANTINE_THRESHOLD="${SYBRA_HEALTH_QUARANTINE_THRESHOLD:-3}"

wait_for_health() {
  local elapsed=0 log_file
  log_file="$(detail_log_path "$ID" health-check)"
  while (( elapsed < TIMEOUT_SEC )); do
    if curl -fsS -m 3 "${CURL_TLS_ARGS[@]}" "$HEALTH_URL" >/dev/null 2>>"$log_file"; then
      return 0
    fi
    sleep "$INTERVAL_SEC"
    elapsed=$((elapsed + INTERVAL_SEC))
  done
  return 1
}

promote() {
  local target
  target="$(resolved_target "$CURRENT_LINK")"
  if [[ -z "$target" ]]; then
    log "current release pointer is missing/broken; nothing to promote"
    return 1
  fi
  atomic_symlink "$target" "$LAST_GOOD_LINK"
  clear_health_failures "$KEY"
  clear_quarantine "$KEY"
  log "release $ID (sha=$SHA) healthy after startup; promoted to last-good"
}

rollback() {
  local last cur
  last="$(resolved_target "$LAST_GOOD_LINK")"
  cur="$(resolved_target "$CURRENT_LINK")"
  if [[ -z "$last" ]]; then
    log "rollback: no last-good release available; service stays down on the failing candidate"
    return 1
  fi
  if [[ "$last" == "$cur" ]]; then
    log "rollback: current already equals last-good; nothing to roll back — last-good itself is failing health checks"
    return 0
  fi
  atomic_symlink "$last" "$CURRENT_LINK"
  log "rollback: restored last-good release; next restart will run it"
}

main() {
  # Identify by the release actually running behind "current" — NOT the source
  # HEAD. After a rollback those diverge, and keying off HEAD would clear the
  # bad candidate's quarantine the moment the healthy rolled-back release
  # answers /health, re-arming a rebuild of the known-bad sha on the next
  # restart. See running_sha in sybra-deploy-lib.sh.
  SHA="$(running_sha)"
  KEY="$(candidate_key "$SHA")"
  ID="${SHA}-post-$$"

  log "waiting up to ${TIMEOUT_SEC}s for $HEALTH_URL to answer"
  if wait_for_health; then
    promote
    return 0
  fi

  log "health check failed after ${TIMEOUT_SEC}s: $HEALTH_URL never answered"
  local count
  count="$(record_health_failure "$KEY")"
  log "candidate sha=$SHA has failed startup health checks $count consecutive time(s)"
  if (( count >= QUARANTINE_THRESHOLD )); then
    write_quarantine "$KEY" "health-check" "failed startup health check $count consecutive time(s)" "$SHA"
  fi
  rollback
  return 1
}

with_deploy_lock main
rc=$?
if [[ $rc -eq 2 ]]; then
  log "lock contention: another deploy is active; leaving current/last-good untouched and failing this start"
  exit 1
fi
exit "$rc"
