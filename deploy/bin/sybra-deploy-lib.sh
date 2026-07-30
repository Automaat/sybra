#!/usr/bin/env bash
# Shared helpers for deploy/bin/sybra-build.sh and deploy/bin/sybra-healthcheck.sh.
# Sourced, not executed — no shebang execution, no `set -e` (callers own that).
#
# Layout this library manages under SYBRA_RELEASES_DIR's parent:
#
#   releases/<sha>-<ts>/        versioned candidate build (server + cli + web)
#   current -> releases/<id>    symlink, the release ExecStart runs
#   last-good -> releases/<id>  symlink, the release healthcheck restores on failure
#   quarantine/<key>.reason     marker for a candidate (source sha + live config
#                                hash) that already failed preflight or health
#                                checks; presence short-circuits a rebuild
#   deploy-state/<key>.failcount  consecutive health-check failure counter
#   deploy-state/<key>.buildfail  consecutive transient build-phase failure
#                                 counter (dependency install / compile) — a
#                                 build phase is only quarantined once this
#                                 crosses its threshold, so a one-off flake
#                                 does not permanently pin the host to the old
#                                 release
#
# LOG_TAG is set by the sourcing script so journal lines are attributable.

: "${LOG_TAG:=sybra-deploy}"

log() { echo "[$LOG_TAG] $*"; }

SRC="${SYBRA_SRC_DIR:-/opt/sybra/src}"
RELEASES_DIR="${SYBRA_RELEASES_DIR:-/opt/sybra/releases}"
CURRENT_LINK="${SYBRA_CURRENT_LINK:-/opt/sybra/current}"
LAST_GOOD_LINK="${SYBRA_LAST_GOOD_LINK:-/opt/sybra/last-good}"
QUARANTINE_DIR="${SYBRA_QUARANTINE_DIR:-/opt/sybra/quarantine}"
STATE_DIR="${SYBRA_DEPLOY_STATE_DIR:-/opt/sybra/deploy-state}"
LOCK_FILE="${SYBRA_DEPLOY_LOCK:-/opt/sybra/deploy.lock}"
LOCK_WAIT_SEC="${SYBRA_DEPLOY_LOCK_WAIT_SEC:-300}"
CONFIG_HOME="${SYBRA_HOME:-${HOME:-/home/sybra}/.sybra}"
CONFIG_FILE="$CONFIG_HOME/config.yaml"

# candidate_sha prints the short SHA of $SRC's current HEAD, or "unknown" if
# $SRC isn't a git checkout (never fails the caller).
candidate_sha() {
  git -C "$SRC" rev-parse --short HEAD 2>/dev/null || echo unknown
}

# config_fingerprint prints a short hash of the live config file's bytes, or
# "absent" if it doesn't exist yet. Never prints config contents — only a
# fingerprint — so callers can log it freely without leaking secrets.
config_fingerprint() {
  if [[ -f "$CONFIG_FILE" ]]; then
    sha256sum "$CONFIG_FILE" | cut -d' ' -f1 | cut -c1-12
  else
    echo absent
  fi
}

# candidate_key derives a stable identity for "this source SHA against this
# live config" so a candidate that was already rejected (bad config, bad
# build, failing health check) is recognized on the next deploy attempt
# without rebuilding it — the acceptance bar for "a rejected candidate does
# not trigger repeated dependency installs or overlapping builds".
candidate_key() {
  local sha="$1"
  printf '%s:%s' "$sha" "$(config_fingerprint)" | sha256sum | cut -d' ' -f1
}

quarantine_file() { echo "$QUARANTINE_DIR/$1.reason"; }
failcount_file() { echo "$STATE_DIR/$1.failcount"; }

is_quarantined() { [[ -f "$(quarantine_file "$1")" ]]; }

# write_quarantine records why a candidate key is blocked. Message must be a
# short, static, operator-facing phrase — never raw command output, which may
# echo config values — so quarantine markers are always safe to paste into a
# log or issue. Full diagnostic detail belongs in detail_log_path, a
# host-local file never shipped anywhere.
write_quarantine() {
  local key="$1" phase="$2" message="$3" sha="$4"
  mkdir -p "$QUARANTINE_DIR"
  {
    echo "timestamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "candidate_sha=$sha"
    echo "phase=$phase"
    echo "reason=$message"
  } >"$(quarantine_file "$key")"
  log "quarantined candidate sha=$sha phase=$phase reason=\"$message\""
}

clear_quarantine() {
  rm -f "$(quarantine_file "$1")"
}

record_health_failure() {
  local key="$1"
  mkdir -p "$STATE_DIR"
  local f count
  f="$(failcount_file "$key")"
  count=0
  [[ -f "$f" ]] && count="$(cat "$f" 2>/dev/null || echo 0)"
  [[ "$count" =~ ^[0-9]+$ ]] || count=0
  count=$((count + 1))
  echo "$count" >"$f"
  echo "$count"
}

clear_health_failures() {
  rm -f "$(failcount_file "$1")"
}

buildfail_file() { echo "$STATE_DIR/$1.buildfail"; }

# record_build_failure bumps and prints the consecutive-failure count for a
# transient build phase (dependency install / compile / smoke). Unlike a
# deterministic phase, these can fail on host/network/toolchain flakes that
# clear on their own, so the caller retries them a few times before
# quarantining — otherwise a one-off outage would block the same sha+config
# forever.
record_build_failure() {
  local key="$1"
  mkdir -p "$STATE_DIR"
  local f count
  f="$(buildfail_file "$key")"
  count=0
  [[ -f "$f" ]] && count="$(cat "$f" 2>/dev/null || echo 0)"
  [[ "$count" =~ ^[0-9]+$ ]] || count=0
  count=$((count + 1))
  echo "$count" >"$f"
  echo "$count"
}

clear_build_failures() {
  rm -f "$(buildfail_file "$1")"
}

# detail_log_path is where a phase can stash full (potentially verbose, but
# still not-secret-bearing) diagnostic output for operator inspection —
# never printed to the journal, never quoted into a quarantine reason.
detail_log_path() {
  local id="$1" phase="$2"
  mkdir -p "$STATE_DIR"
  echo "$STATE_DIR/$id.$phase.log"
}

# atomic_symlink repoints link -> target without ever leaving link missing or
# pointing at a half-written path. `mv -T` (not a bare `mv`) is required:
# without it, if link already exists as a symlink to a directory, plain `mv`
# moves the new symlink *into* that directory instead of replacing link.
atomic_symlink() {
  local target="$1" link="$2" tmp
  tmp="$link.tmp.$$"
  ln -sfn "$target" "$tmp"
  mv -T "$tmp" "$link"
}

resolved_target() {
  [[ -e "$1" || -L "$1" ]] && readlink -f "$1"
}

# with_deploy_lock runs "$@" while holding an exclusive flock on LOCK_FILE, so
# a manually-invoked deploy can never overlap an in-flight systemd-driven one
# (or vice versa). Returns 2 (distinct from a real command failure) if the
# lock isn't acquired within LOCK_WAIT_SEC — callers treat that as
# lock-contention, not a build/preflight/health failure.
with_deploy_lock() {
  mkdir -p "$(dirname "$LOCK_FILE")"
  exec 200>"$LOCK_FILE"
  if ! flock -w "$LOCK_WAIT_SEC" 200; then
    log "lock contention: another deploy held $LOCK_FILE for over ${LOCK_WAIT_SEC}s"
    return 2
  fi
  "$@"
}
