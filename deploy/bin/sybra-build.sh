#!/usr/bin/env bash
set -uo pipefail

SRC="${SYBRA_SRC_DIR:-/opt/sybra/src}"
OUT="${SYBRA_BUILD_DIR:-/opt/sybra/build}"
BIN="$OUT/sybra-server"
CLI_BIN="$OUT/sybra-cli"
WEB="$OUT/web"
CLI_LINK_DIR="${HOME:-/home/sybra}/.local/bin"

log() { echo "[sybra-build] $*"; }

have_last_good_build() { [[ -x "$BIN" && -f "$WEB/index.html" ]]; }

keep_last_good_or_fail() {
  log "build failed: $1"
  if have_last_good_build; then
    log "keeping last-good build; service will start on the previous version"
    exit 0
  fi
  log "no prior good build to fall back on; failing the unit"
  exit 1
}

command -v mise >/dev/null 2>&1 || keep_last_good_or_fail "mise not on PATH"
cd "$SRC" || keep_last_good_or_fail "source dir $SRC missing"

mise install || keep_last_good_or_fail "mise install"

log "building web bundle at $(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
( cd frontend && mise exec -- npm ci --no-audit --no-fund && mise exec -- npm run build:web ) \
  || keep_last_good_or_fail "frontend build"

log "building server binary"
mkdir -p "$OUT"
if ! CGO_ENABLED=0 mise exec -- go build -trimpath -o "$BIN.new" ./cmd/sybra-server; then
  rm -f "$BIN.new"
  keep_last_good_or_fail "go build"
fi

# Built alongside sybra-server, from the same source checkout, in the same
# atomic swap below — this is what keeps the CLI's config-schema handling
# from drifting out of sync with the running server (#2619).
log "building sybra-cli binary"
if ! CGO_ENABLED=0 mise exec -- go build -trimpath -o "$CLI_BIN.new" ./cmd/sybra-cli; then
  rm -f "$BIN.new" "$CLI_BIN.new"
  keep_last_good_or_fail "go build sybra-cli"
fi

if command -v bwrap >/dev/null 2>&1; then
  log "running linked-worktree sandbox smoke"
  if ! mise exec -- go test ./internal/agent -run '^TestSandboxEnforce_LinkedWorktreeGitOps$' -count=1; then
    rm -f "$BIN.new" "$CLI_BIN.new"
    keep_last_good_or_fail "linux sandbox git smoke"
  fi
fi

rm -rf "$WEB.new"
if ! cp -a frontend/dist-web "$WEB.new"; then
  rm -f "$BIN.new" "$CLI_BIN.new"
  keep_last_good_or_fail "stage web bundle"
fi
rm -rf "$WEB"
if ! mv "$WEB.new" "$WEB"; then
  keep_last_good_or_fail "swap web bundle"
fi
if ! mv -f "$BIN.new" "$BIN"; then
  keep_last_good_or_fail "swap server binary"
fi
if ! mv -f "$CLI_BIN.new" "$CLI_BIN"; then
  keep_last_good_or_fail "swap sybra-cli binary"
fi

if ! (mkdir -p "$CLI_LINK_DIR" && ln -sf "$CLI_BIN" "$CLI_LINK_DIR/sybra-cli"); then
  log "warning: failed to symlink sybra-cli into $CLI_LINK_DIR; binary is still at $CLI_BIN"
fi

log "build complete: $(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
