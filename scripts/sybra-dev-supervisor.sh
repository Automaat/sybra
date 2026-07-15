#!/usr/bin/env bash
set -u

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root" || exit 1

sybra_home="${SYBRA_HOME:-$HOME/.sybra}"
restart_marker="$sybra_home/restart-requested"
mkdir -p "$sybra_home"
rm -f "$restart_marker"

follower_token_file="$sybra_home/home-nas-follower.token"
if [ -z "${HOME_NAS_SYBRA_TOKEN:-}" ] && [ -f "$follower_token_file" ]; then
  HOME_NAS_SYBRA_TOKEN="$(cat "$follower_token_file")"
  export HOME_NAS_SYBRA_TOKEN
fi

sync_deps() {
  mise install || return $?
  if [ -f frontend/package-lock.json ]; then
    if [ ! -f frontend/node_modules/.package-lock.json ] || [ frontend/package-lock.json -nt frontend/node_modules/.package-lock.json ]; then
      (cd frontend && npm ci) || return $?
    fi
  fi
}

while true; do
  sync_deps || exit $?
  mise run dev
  status=$?

  if [ -f "$restart_marker" ]; then
    rm -f "$restart_marker"
    echo "Sybra requested restart after auto-update; rebuilding..."
    continue
  fi

  if [ "$status" -eq 42 ]; then
    echo "Sybra requested restart after auto-update; rebuilding..."
    continue
  fi

  exit "$status"
done
