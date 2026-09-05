#!/usr/bin/env bash
# ExecStart for the sybra systemd unit. Runs whatever release sybra-build.sh
# (ExecStartPre) most recently activated — $CURRENT_LINK is a symlink that
# sybra-build.sh and sybra-healthcheck.sh repoint atomically, so this script
# never itself decides which release is active.
set -euo pipefail

SRC="${SYBRA_SRC_DIR:-/opt/sybra/src}"
OUT="${SYBRA_CURRENT_LINK:-/opt/sybra/current}"
export SYBRA_STATIC_DIR="${SYBRA_STATIC_DIR:-$OUT/web}"

cd "$SRC"
exec mise exec -- "$OUT/sybra-server"
