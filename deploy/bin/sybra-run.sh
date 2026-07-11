#!/usr/bin/env bash
set -euo pipefail

SRC="${SYBRA_SRC_DIR:-/opt/sybra/src}"
OUT="${SYBRA_BUILD_DIR:-/opt/sybra/build}"
export SYBRA_STATIC_DIR="${SYBRA_STATIC_DIR:-$OUT/web}"

cd "$SRC"
exec mise exec -- "$OUT/sybra-server"
