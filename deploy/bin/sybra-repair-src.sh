#!/usr/bin/env bash
# Privileged ExecStartPre for the sybra systemd unit. Repairs ownership drift
# in the source checkout before the unprivileged build/autoupdate path touches
# .git/objects. This exists because the service itself runs as sybra and
# cannot chown a checkout polluted by a root-run manual git command.
set -uo pipefail

LOG_TAG=sybra-repair-src
log() { echo "[$LOG_TAG] $*"; }

SRC="${SYBRA_SRC_DIR:-/opt/sybra/src}"
SERVICE_USER="${SYBRA_SERVICE_USER:-sybra}"
SERVICE_GROUP="${SYBRA_SERVICE_GROUP:-$SERVICE_USER}"

if [[ ! -e "$SRC" ]]; then
  log "source dir $SRC missing; build preflight will report the hard failure"
  exit 0
fi

if ! id "$SERVICE_USER" >/dev/null 2>&1; then
  log "service user $SERVICE_USER does not exist; skipping repair"
  exit 0
fi
if command -v getent >/dev/null 2>&1 && ! getent group "$SERVICE_GROUP" >/dev/null 2>&1; then
  log "service group $SERVICE_GROUP does not exist; skipping repair"
  exit 0
fi

real_src="$(readlink -f "$SRC" 2>/dev/null || true)"
case "$real_src" in
/opt/sybra/src | /opt/sybra/src/*)
  ;;
*)
  if [[ "${SYBRA_REPAIR_ALLOW_ANY_SRC:-}" != "1" ]]; then
    log "refusing to chown unexpected source path $real_src; set SYBRA_REPAIR_ALLOW_ANY_SRC=1 only in an isolated test/deploy harness"
    exit 0
  fi
  ;;
esac

if [[ ! -d "$real_src/.git" && ! -f "$real_src/.git" ]]; then
  log "$real_src is not a git checkout; build preflight will report the hard failure"
  exit 0
fi

drift_path=""
find_err=""
if ! find_out="$(find "$real_src" \( ! -user "$SERVICE_USER" -o ! -group "$SERVICE_GROUP" \) -print -quit 2>&1)"; then
  find_err="$find_out"
else
  drift_path="$find_out"
fi
if [[ -n "$find_err" ]]; then
  log "ownership scan failed for $real_src: $find_err"
  exit 0
fi

if [[ -n "$drift_path" ]]; then
	if [[ "$(id -u)" != "0" ]]; then
		log "not running as root; cannot repair ownership drift for $real_src (first drift: $drift_path)"
		exit 0
	fi
	log "repairing source ownership under $real_src to $SERVICE_USER:$SERVICE_GROUP"
	chown -R "$SERVICE_USER:$SERVICE_GROUP" "$real_src"
else
  log "source ownership already correct"
fi
