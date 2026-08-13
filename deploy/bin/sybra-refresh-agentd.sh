#!/usr/bin/env bash
# ExecStartPost hook for sybra.service. If the optional local agent daemon is
# installed and enabled, restart it asynchronously after the new leader release
# has passed its health check. This makes an exit-42 autoupdate activate the
# matching agentd binary without making sybra-server depend on agentd.
set -euo pipefail

UNIT="${SYBRA_AGENTD_UNIT:-sybra-agentd.service}"
CONFIG="${SYBRA_AGENTD_CONFIG:-/etc/sybra/sybra-agentd.yaml}"

if [[ -f "$CONFIG" ]] && systemctl is-enabled --quiet "$UNIT"; then
  systemctl --no-block restart "$UNIT"
fi
