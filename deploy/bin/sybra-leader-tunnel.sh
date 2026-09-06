#!/usr/bin/env bash
# Run on the leader laptop under launchd. Reconcile the reverse tunnel with
# Sybra's persisted desktop listener port; an alive SSH session is not proof
# that the application behind it is reachable.
set -euo pipefail

if [[ $# != 3 || "$1" != /* || "$2" == -* || -z "$2" ]]; then
  echo "usage: sybra-leader-tunnel.sh ABSOLUTE_SYBRA_HOME SSH_TARGET REMOTE_PORT" >&2
  exit 2
fi
LEADER_HOME="$1"
SSH_TARGET="$2"
REMOTE_PORT="$3"
valid_port() { [[ "$1" =~ ^[1-9][0-9]{0,4}$ ]] && (( 10#$1 <= 65535 )); }
valid_port "$REMOTE_PORT" || { echo "invalid remote port" >&2; exit 2; }
command -v jq >/dev/null || { echo "jq is required to validate the leader identity" >&2; exit 2; }
# Match httpserve.HomeID: absolute, symlink-resolved, cleaned home path.
LEADER_HOME="$(cd "$LEADER_HOME" && pwd -P)"
LEADER_ID="$(printf '%s' "$LEADER_HOME" | shasum -a 256 | awk '{print $1}')"

healthy_leader() {
  local response
  response="$(curl --noproxy '*' -fsS --max-time 2 --max-filesize 4096 \
    "http://127.0.0.1:$1/health")" || return 1
  jq -e --arg home "$LEADER_ID" \
    '.status == "ok" and .service == "sybra" and .home_id == $home' \
    <<<"$response" >/dev/null
}

tunnel_pid=""
tunnel_port=""
stop_tunnel() {
  if [[ -n "$tunnel_pid" ]]; then
    kill "$tunnel_pid" 2>/dev/null || true
    wait "$tunnel_pid" 2>/dev/null || true
    tunnel_pid=""
    tunnel_port=""
  fi
}
trap stop_tunnel EXIT
trap 'exit 0' INT TERM

while true; do
  port=""
  if [[ -r "$LEADER_HOME/desktop-port" ]]; then
    IFS= read -r port < "$LEADER_HOME/desktop-port" || true
  fi
  if ! valid_port "$port" || ! healthy_leader "$port" 2>/dev/null; then
    if [[ -n "$tunnel_pid" ]]; then
      echo "[sybra-tunnel] leader unavailable; closing stale forward" >&2
    fi
    stop_tunnel
  elif [[ "$port" != "$tunnel_port" ]] || ! kill -0 "$tunnel_pid" 2>/dev/null; then
    stop_tunnel
    echo "[sybra-tunnel] forwarding to healthy leader port $port" >&2
    ssh -nNT -o BatchMode=yes -o ExitOnForwardFailure=yes -o ConnectTimeout=10 \
      -o ServerAliveInterval=15 -o ServerAliveCountMax=2 \
      -R "127.0.0.1:$REMOTE_PORT:127.0.0.1:$port" "$SSH_TARGET" &
    tunnel_pid=$!
    tunnel_port="$port"
  fi
  sleep 5
done
