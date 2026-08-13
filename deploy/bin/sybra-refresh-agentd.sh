#!/usr/bin/env bash
# ExecStartPost hook for sybra.service. If the optional local agent daemon is
# installed and enabled, restart it asynchronously after the new leader release
# has passed its health check. This makes an exit-42 autoupdate activate the
# matching agentd binary without making sybra-server depend on agentd.
set -euo pipefail

UNIT="${SYBRA_AGENTD_UNIT:-sybra-agentd.service}"
CONFIG="${SYBRA_AGENTD_CONFIG:-/etc/sybra/sybra-agentd.yaml}"
BINARY="${SYBRA_AGENTD_BINARY:-/opt/sybra/current/sybra-agentd}"

# An initial rollout can health-check and roll back to a last-good release that
# predates agentd. Leave the durable daemon stopped in that case; the next
# successful agentd-capable activation will restart it.
if [[ -f "$CONFIG" && -x "$BINARY" ]] && systemctl is-enabled --quiet "$UNIT"; then
  systemctl --no-block restart "$UNIT"
fi
