#!/usr/bin/env bash
# Run sybra-server against an isolated, seeded dev home with fake provider CLIs.
# Pair with `mise run dev:web` (vite HMR) in another terminal — the vite proxy
# forwards /api and /events here. No model credits are ever spent.
#
#   scripts/dev-mock/backend.sh            # start server on :8080
#   scripts/dev-mock/backend.sh --reset    # wipe dev home, re-seed, start
#
# Env:
#   SYBRA_PORT   listen port (default 8080)
#   DEV_HOME     dev home dir (default .dev-mock/home under repo root)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DEV_ROOT="${DEV_ROOT:-$REPO_ROOT/.dev-mock}"
DEV_HOME="${DEV_HOME:-$DEV_ROOT/home}"
FAKEBIN="$DEV_ROOT/fakebin"

if [ "${1:-}" = "--reset" ]; then
  echo "Resetting dev home at $DEV_HOME"
  rm -rf "$DEV_HOME"
fi

mkdir -p "$DEV_HOME/logs" "$FAKEBIN"

# --- fake provider CLIs (emit valid stream-json, spend nothing) ------------
cat >"$FAKEBIN/claude" <<'SH'
#!/usr/bin/env bash
printf '{"type":"system","session_id":"fake-claude-session"}\n'
printf '{"type":"assistant","message":{"content":[{"type":"text","text":"fake claude working"}]}}\n'
printf '{"type":"result","subtype":"success","session_id":"fake-claude-session","result":"claude done","total_cost_usd":0.0123,"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}\n'
SH

cat >"$FAKEBIN/codex" <<'SH'
#!/usr/bin/env bash
printf '{"type":"thread.started","thread_id":"fake-codex-session"}\n'
printf '{"type":"item.completed","item":{"type":"agent_message","text":"codex done"}}\n'
SH

cat >"$FAKEBIN/copilot" <<'SH'
#!/usr/bin/env bash
printf '{"type":"result","sessionId":"fake-copilot-session","exitCode":0,"usage":{"premiumRequests":7.5,"sessionDurationMs":100,"totalApiDurationMs":50}}\n'
SH

chmod +x "$FAKEBIN/claude" "$FAKEBIN/codex" "$FAKEBIN/copilot"

# --- seed mock data (idempotent) -------------------------------------------
SYBRA_HOME="$DEV_HOME" "$REPO_ROOT/scripts/dev-mock/seed.sh"

# --- run the server --------------------------------------------------------
echo "sybra-server → http://localhost:${SYBRA_PORT:-8080}  (home: $DEV_HOME)"
echo "Now run 'mise run dev:web' in another terminal for the HMR frontend."
cd "$REPO_ROOT"
exec env \
  PATH="$FAKEBIN:$PATH" \
  SYBRA_HOME="$DEV_HOME" \
  go run ./cmd/sybra-server
