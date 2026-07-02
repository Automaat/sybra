#!/usr/bin/env bash
# Enforces the Dockerfile cache strategy documented at the top of the
# runtime stage. Heavy, version-pinned tool layers (apt, npm globals,
# klaudiush, mise) MUST appear before the thin per-commit layers (sybra
# binaries + web assets). Re-ordering these silently invalidates the
# cache on every sybra bump and bloats pull-time on re-deploys.
#
# Hadolint cannot detect this — it lints individual instructions, not
# the *position* of heavy work relative to thin work. This script does.

set -euo pipefail

DOCKERFILE="${1:-Dockerfile}"

if [[ ! -f "${DOCKERFILE}" ]]; then
  echo "::error::${DOCKERFILE} not found" >&2
  exit 1
fi

fail=0

note() {
  echo "  - $*" >&2
}

err() {
  echo "::error file=${DOCKERFILE}::$*" >&2
  fail=1
}

# --- 1. FROM pins ---------------------------------------------------------
# Every FROM must use @sha256:<digest>. Tag-only FROMs let a base-image
# republish silently shift the runtime out from under us.
while IFS= read -r line; do
  if [[ ! "${line}" =~ @sha256:[a-f0-9]{64} ]]; then
    err "FROM without sha256 digest pin: ${line}"
  fi
done < <(grep -E '^FROM ' "${DOCKERFILE}")

# --- 2. Layer marker ordering --------------------------------------------
# The runtime stage uses ordered markers (Layer A, B, C, D, E, F+G) so a
# human reviewer can spot reorderings in a diff. Enforce that they appear
# in alphabetical order and each appears exactly once.
expected=(A B C D E "F\\+G")
prev_line=0
for marker in "${expected[@]}"; do
  count=$(grep -cE "^# --- Layer ${marker}:" "${DOCKERFILE}" || true)
  if [[ "${count}" -ne 1 ]]; then
    err "expected exactly one '# --- Layer ${marker}:' marker, found ${count}"
    continue
  fi
  this_line=$(grep -nE "^# --- Layer ${marker}:" "${DOCKERFILE}" | head -1 | cut -d: -f1)
  if (( this_line <= prev_line )); then
    err "Layer ${marker} (line ${this_line}) appears before previous marker (line ${prev_line})"
  fi
  prev_line="${this_line}"
done

# --- 3. Heavy operations confined to their layers ------------------------
# The whole point of the layer order is that heavy/unpinned operations
# never appear in the thin per-commit zone. If someone adds `RUN apt-get
# install` to Layer F+G, the apt cache is invalidated on every sybra
# bump and the image regrows by ~150MB silently. Catch that here.
#
# Strategy: read the file, track which layer marker we're under, and
# flag rule violations per layer.
awk -v dockerfile="${DOCKERFILE}" '
  /^# --- Layer A:/   { layer="A"; next }
  /^# --- Layer B:/   { layer="B"; next }
  /^# --- Layer C:/   { layer="C"; next }
  /^# --- Layer D:/   { layer="D"; next }
  /^# --- Layer E:/   { layer="E"; next }
  /^# --- Layer F\+G:/{ layer="FG"; next }

  # apt-get install only allowed in Layer A.
  /^[[:space:]]*&&[[:space:]]*apt-get[[:space:]]+install/ || /apt-get[[:space:]]+install/ {
    if (layer != "" && layer != "A") {
      printf("::error file=%s,line=%d::apt-get install outside Layer A (in Layer %s): %s\n",
             dockerfile, NR, layer, $0) > "/dev/stderr"
      bad=1
    }
  }

  # npm install -g only allowed in Layer C.
  /npm[[:space:]]+install[[:space:]]+-g/ {
    if (layer != "" && layer != "C") {
      printf("::error file=%s,line=%d::npm install -g outside Layer C (in Layer %s): %s\n",
             dockerfile, NR, layer, $0) > "/dev/stderr"
      bad=1
    }
  }

  # COPY --from=<builder> only allowed in Layer F+G (the thin per-commit zone).
  /^COPY[[:space:]]+--from=/ {
    if (layer != "" && layer != "FG") {
      printf("::error file=%s,line=%d::COPY --from=<builder> outside Layer F+G (in Layer %s): %s\n",
             dockerfile, NR, layer, $0) > "/dev/stderr"
      bad=1
    }
  }

  END { exit bad ? 1 : 0 }
' "${DOCKERFILE}" || fail=1

# --- 4. No unverified remote installer pipes ------------------------------
# `curl ... | sh` (or `| bash`) executes a mutable remote script with no
# pinning or checksum verification — a changed or compromised endpoint can
# alter the image with no diff in this repo. Require direct artifact
# downloads verified against a checksum instead (see Layer B, Layer D).
while IFS=: read -r lineno content; do
  err "unverified remote installer pipe (curl|sh / curl|bash) at line ${lineno}: ${content}"
done < <(grep -nE '\|[[:space:]]*(sudo[[:space:]]+)?(sh|bash)([[:space:]]|$)' "${DOCKERFILE}" || true)

# --- 5. Healthcheck uses curl, not node ----------------------------------
# Node-based healthchecks are fragile: they depend on the node runtime
# being on PATH inside the runtime stage. curl is installed in Layer A
# and is the portable choice. HEALTHCHECK spans line-continuations, so
# grep -A1 picks up the CMD line that follows.
hc=$(grep -A1 -E "^HEALTHCHECK" "${DOCKERFILE}" || true)
if printf "%s" "${hc}" | grep -q "node -e"; then
  err "HEALTHCHECK still uses node -e, prefer curl for portability"
fi

if [[ "${fail}" -ne 0 ]]; then
  echo "" >&2
  echo "Dockerfile layer check FAILED. See errors above." >&2
  exit 1
fi

echo "Dockerfile layer check passed."
