#!/usr/bin/env bash
# Keep sqlite-only SQL from reaching an operator's postgres box unreviewed.
#
# The postgres half of every store's behaviour suite skips unless a server is configured, so a local `go test ./...` covers one engine and looks complete. This mirrors CI's "Go Database Engine Tests" job on demand. It is not a step in `mise run verify` because it needs a container runtime, and verify has to pass on the deploy LXC, which has mise and nothing else.

set -euo pipefail

cd "$(dirname "$0")/.."

CONTAINER="${SYBRA_TEST_PG_CONTAINER:-sybra-test-postgres}"
PORT="${SYBRA_TEST_PG_PORT:-55432}"
IMAGE="postgres:17-alpine@sha256:742f40ea20b9ff2ff31db5458d127452988a2164df9e17441e191f3b72252193"
PACKAGES=("./internal/db/..." "./internal/loopagent/...")

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required to run the postgres engine leg" >&2
  exit 1
fi

cleanup() {
  docker rm -f "${CONTAINER}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

cleanup
docker run -d --name "${CONTAINER}" \
  -e POSTGRES_USER=sybra -e POSTGRES_PASSWORD=sybra -e POSTGRES_DB=sybra \
  -p "${PORT}:5432" "${IMAGE}" >/dev/null

echo "waiting for postgres on :${PORT}"
for _ in $(seq 1 60); do
  if docker exec "${CONTAINER}" pg_isready -U sybra -d sybra >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
if ! docker exec "${CONTAINER}" pg_isready -U sybra -d sybra >/dev/null 2>&1; then
  echo "postgres did not become ready" >&2
  docker logs "${CONTAINER}" >&2 || true
  exit 1
fi

SYBRA_TEST_POSTGRES_DSN="postgres://sybra:sybra@127.0.0.1:${PORT}/sybra?sslmode=disable" \
  SYBRA_REQUIRE_POSTGRES_TESTS=1 \
  go test -race -count=1 -timeout 10m "${PACKAGES[@]}"
