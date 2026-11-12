#!/usr/bin/env bash
# One-time migration of an enrolled worker updater from /opt/sybra to the
# root-owned /usr/local/lib/sybra tree.
#
# The updater and its GitHub App helper refuse every path with a non-root or
# group/world-writable ancestor. On a co-hosted board /opt/sybra must stay
# owned by the sybra account (sybra.service swaps /opt/sybra/current), so the
# root-run updater can never be trusted there.
#
# Usage: as root, from a reviewed checkout, with gh able to read this public
# repository's attestations (for example GH_TOKEN exported for this shell).
# sybra-agentd is not restarted and nothing under /opt/sybra is deleted.
set -euo pipefail

REPO="${SYBRA_REPOSITORY:-Automaat/sybra}"
OLD_ROOT="${SYBRA_OLD_WORKER_ROOT:-/opt/sybra}"
NEW_ROOT="${SYBRA_WORKER_ROOT:-/usr/local/lib/sybra}"
UPDATE_CONFIG="${SYBRA_WORKER_UPDATE_CONFIG:-/etc/sybra/worker-update.yaml}"
JOURNAL="${SYBRA_WORKER_UPDATE_JOURNAL:-/var/lib/sybra-worker-update/update.json}"
UNIT_DIR="${SYBRA_SYSTEMD_DIR:-/etc/systemd/system}"
DEPLOY_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

die() {
  echo "sybra-worker-update-migrate: $*" >&2
  exit 1
}

[[ "$(id -u)" == 0 ]] || die "run as root"

# An in-flight operation holds the worker and its journal names old paths.
if [[ -e "$JOURNAL" ]]; then
  phase="$(grep -o '"phase":"[a-z-]*"' "$JOURNAL" | head -n 1 | cut -d'"' -f4 || true)"
  case "$phase" in
    "" | complete | retired | quarantined) ;;
    *) die "update operation in phase '$phase' is in flight; let it finish first" ;;
  esac
fi

target="$(readlink "$OLD_ROOT/worker-current")" || die "$OLD_ROOT/worker-current is not a symlink"
revision="$(basename "$target")"
[[ "$revision" =~ ^[0-9a-f]{40}$ ]] || die "current worker target is not a full SHA release"
[[ "$target" == "$OLD_ROOT/worker-releases/$revision" ]] || die "current worker target is outside $OLD_ROOT/worker-releases"

systemctl stop sybra-worker-update.timer sybra-worker-update.service || true

install -d -o root -g root -m 0755 "$NEW_ROOT" "$NEW_ROOT/bin" "$NEW_ROOT/worker" "$NEW_ROOT/worker/releases"

release="$NEW_ROOT/worker/releases/$revision"
if [[ ! -d "$release" ]]; then
  stage="$NEW_ROOT/worker/releases/.migrate-$revision"
  rm -rf -- "$stage"
  install -d -o root -g root -m 0755 "$stage"
  for name in sybra-agentd sybra-worker-update; do
    [[ -f "$target/$name" && ! -L "$target/$name" ]] || die "$target/$name is missing"
    install -o root -g root -m 0755 "$target/$name" "$stage/$name"
    # The old tree was writable by the service account: its bytes need the
    # same provenance proof the updater demands for a new release.
    gh attestation verify "$stage/$name" --repo "$REPO" \
      --signer-workflow "$REPO/.github/workflows/ci.yml" \
      --source-ref refs/heads/main --source-digest "$revision" \
      --signer-digest "$revision" --deny-self-hosted-runners --hostname github.com >/dev/null
  done
  sync "$stage"
  mv -T "$stage" "$release"
fi

install -o root -g root -m 0755 "$release/sybra-worker-update" "$NEW_ROOT/bin/sybra-worker-update"
install -o root -g root -m 0755 "$DEPLOY_DIR/bin/sybra-worker-github-auth.py" "$NEW_ROOT/bin/sybra-worker-github-auth.py"

ln -sfn "$release" "$NEW_ROOT/worker/current.migrating"
mv -T "$NEW_ROOT/worker/current.migrating" "$NEW_ROOT/worker/current"

sed -i \
  -e "s#^release_root: .*#release_root: $NEW_ROOT/worker/releases#" \
  -e "s#^current_link: .*#current_link: $NEW_ROOT/worker/current#" \
  "$UPDATE_CONFIG"

install -o root -g root -m 0644 "$DEPLOY_DIR/systemd/sybra-worker-update.service" "$UNIT_DIR/sybra-worker-update.service"
install -o root -g root -m 0644 "$DEPLOY_DIR/systemd/sybra-worker-update.timer" "$UNIT_DIR/sybra-worker-update.timer"
if [[ -e "$UNIT_DIR/sybra-worker-update.service.d/zz-github-app.conf" ]]; then
  install -o root -g root -m 0644 "$DEPLOY_DIR/systemd/sybra-worker-update-github-app.conf" \
    "$UNIT_DIR/sybra-worker-update.service.d/zz-github-app.conf"
fi
if [[ -e "$UNIT_DIR/sybra-agentd.service.d/standalone.conf" ]]; then
  install -o root -g root -m 0644 "$DEPLOY_DIR/systemd/sybra-agentd-standalone.conf" \
    "$UNIT_DIR/sybra-agentd.service.d/standalone.conf"
fi
systemctl daemon-reload

cat <<EOF
Migrated worker release $revision to $NEW_ROOT/worker/current.
The updater refuses to act until the running agentd matches the new unit.
When the worker has no accepted runs:
  systemctl restart sybra-agentd
  systemctl start sybra-worker-update.timer
The old $OLD_ROOT/worker-releases, $OLD_ROOT/worker-current and
$OLD_ROOT/bin/sybra-worker-* are unused now; remove them after verifying.
EOF
