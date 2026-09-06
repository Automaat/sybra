#!/usr/bin/env bash
# ExecStartPost hook for sybra.service. If the optional local agent daemon is
# installed and enabled, restart it asynchronously after the new leader release
# has passed its health check. This makes an exit-42 autoupdate activate the
# matching agentd binary without making sybra-server depend on agentd.
set -euo pipefail

UNIT="${SYBRA_AGENTD_UNIT:-sybra-agentd.service}"
CONFIG="${SYBRA_AGENTD_CONFIG:-/etc/sybra/sybra-agentd.yaml}"
BINARY="${SYBRA_AGENTD_BINARY:-/opt/sybra/current/sybra-agentd}"
STANDALONE="${SYBRA_AGENTD_STANDALONE:-/etc/systemd/system/sybra-agentd.service.d/standalone.conf}"

# A worker-only installation has its own release pointer and supervisor.
# An optional old board on the host must never restart it during migration.
[[ ! -f "$STANDALONE" ]] || exit 0

# An initial rollout can health-check and roll back to a last-good release that
# predates agentd. Leave the durable daemon stopped in that case; the next
# successful agentd-capable activation will restart it.
if [[ -f "$CONFIG" && -x "$BINARY" ]] && systemctl is-enabled --quiet "$UNIT"; then
  # An independently starting worker may exhaust its start budget while the
  # board builds its first agentd-capable release. A healthy new release is a
  # fresh start opportunity; do not leave that worker permanently rate-limited.
  # The final retry delay is activating/auto-restart, not failed. Do not query
  # LoadState first either: an inactive unit can be unloaded between commands.
  # An unloaded unit has no retained budget; still require the explicit restart
  # below to succeed. Other reset errors remain visible instead of being hidden.
  if ! systemctl reset-failed "$UNIT"; then
    echo "[sybra-agentd] start-budget reset unavailable; attempting explicit restart" >&2
  fi
  systemctl --no-block restart "$UNIT"
fi
