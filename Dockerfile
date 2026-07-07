# Stage 1: Build web frontend
FROM node:24-slim@sha256:b31e7a42fdf8b8aa5f5ed477c72d694301273f1069c5a2f71d53c6482e99a2fc AS frontend-builder
WORKDIR /app/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build:web

# Stage 2: Build sybra-server binary
FROM golang:1.26.4-bookworm@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b AS go-builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/sybra-server ./cmd/sybra-server \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/sybra-cli ./cmd/sybra-cli

# Stage 3: Runtime — node:24-slim for claude CLI (Node.js-based)
#
# Layer cache strategy (DO NOT REORDER without updating
# scripts/check-dockerfile-layers.sh):
#
#   A. apt system packages + gh repo   — heaviest, rare changes
#   B. klaudiush binary                — pinned via ARG, rare
#   C. node CLIs (claude, codex)       — pinned via ARG, monthly bumps
#   D. mise binary                     — pinned via ARG, rare
#   E. non-root user + static config   — never changes
#   F+G. sybra binaries + web assets   — per-commit, thin (~20MB)
#
# Invariants enforced by scripts/check-dockerfile-layers.sh in CI:
#   - Each "# --- Layer X:" marker appears exactly once, in order
#   - `apt-get install` only inside Layer A
#   - `npm install -g` only inside Layer C
#   - `COPY --from=<builder>` only inside Layer F+G
#   - HEALTHCHECK uses curl (not node -e)
#   - Every FROM is sha256-pinned
#   - No `curl|sh`/`curl|bash` remote-installer pipes (verify checksums instead)
#
# Why it matters: bumping sybra invalidates only the last COPY layers;
# bumping a tool ARG invalidates just that tool's layer. If `apt-get
# install` ever lands in Layer F+G, the apt cache silently regenerates
# on every sybra commit and image size balloons. The linter catches that
# before it reaches main.
FROM node:24-slim@sha256:b31e7a42fdf8b8aa5f5ed477c72d694301273f1069c5a2f71d53c6482e99a2fc AS runtime

# Pipe failures in subsequent RUN blocks should fail the build.
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# Non-root runtime user. Claude CLI refuses --dangerously-skip-permissions
# under uid 0; running as uid 1000 avoids the IS_SANDBOX env-var workaround.
ARG SYBRA_UID=1000
ARG SYBRA_GID=1000

# --- Layer A: apt system packages + gh repo ---
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git curl gpg \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
         | gpg --dearmor -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
         > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends gh \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# --- Layer B: klaudiush binary ---
# Downloads the release archive directly from GitHub and verifies it
# against the release's published checksums.txt, instead of piping the
# klaudiu.sh installer script into `sh` (an unpinned, unverified remote
# script that could change or be compromised without any diff in this
# repo).
# renovate: datasource=github-releases depName=smykla-skalski/klaudiush
ARG KLAUDIUSH_VERSION=v1.36.0
RUN ARCH="$(dpkg --print-architecture)" \
    && case "${ARCH}" in \
         amd64|arm64) KLAUDIUSH_ARCH="${ARCH}" ;; \
         *) echo "unsupported arch: ${ARCH}" >&2 && exit 1 ;; \
       esac \
    && VERSION_NUM="${KLAUDIUSH_VERSION#v}" \
    && ARCHIVE="klaudiush_${VERSION_NUM}_linux_${KLAUDIUSH_ARCH}.tar.gz" \
    && BASE_URL="https://github.com/smykla-skalski/klaudiush/releases/download/${KLAUDIUSH_VERSION}" \
    && TMPDIR="$(mktemp -d)" \
    && curl -sSfL -o "${TMPDIR}/${ARCHIVE}" "${BASE_URL}/${ARCHIVE}" \
    && curl -sSfL -o "${TMPDIR}/checksums.txt" "${BASE_URL}/checksums.txt" \
    && grep "  ${ARCHIVE}\$" "${TMPDIR}/checksums.txt" \
       | sed "s#  ${ARCHIVE}\$#  ${TMPDIR}/${ARCHIVE}#" \
       | sha256sum -c - \
    && tar -xzf "${TMPDIR}/${ARCHIVE}" -C "${TMPDIR}" klaudiush \
    && install -m 0755 "${TMPDIR}/klaudiush" /usr/local/bin/klaudiush \
    && rm -rf "${TMPDIR}"

# --- Layer C: node CLIs (claude code + codex), pinned for cache stability ---
# renovate: datasource=npm depName=@anthropic-ai/claude-code
ARG CLAUDE_CODE_VERSION=2.1.196
# renovate: datasource=npm depName=@openai/codex
ARG CODEX_VERSION=0.142.4
RUN npm install -g \
        "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
        "@openai/codex@${CODEX_VERSION}" \
    && rm -rf /root/.npm

# --- Layer D: mise binary only (tools installed per-worktree) ---
# The prod image intentionally does NOT bake language toolchains. Each
# project declares its tools (e.g. sybra's mise.toml pins Go/Node/etc.)
# and Sybra runs `mise install` in every worktree via SetupCommands on
# creation — cached in ~/.sybra/mise-data, shared across worktrees, version
# pinned per branch. This keeps the image lean and supports projects in any
# language without Dockerfile rebuilds.
#
# Projects that don't use mise leave their SetupCommands pointing at their
# own tool (npm ci, uv sync, cargo build, ./.sybra/bootstrap.sh …).
#
# renovate: datasource=github-releases depName=jdx/mise
ARG MISE_VERSION=v2026.7.0
RUN ARCH="$(dpkg --print-architecture)" \
    && case "${ARCH}" in \
         amd64) MISE_ARCH=x64 ;; \
         arm64) MISE_ARCH=arm64 ;; \
         *) echo "unsupported arch: ${ARCH}" >&2 && exit 1 ;; \
       esac \
    && curl -sSfL -o /usr/local/bin/mise \
         "https://github.com/jdx/mise/releases/download/${MISE_VERSION}/mise-${MISE_VERSION}-linux-${MISE_ARCH}" \
    && chmod +x /usr/local/bin/mise

# --- Layer E: non-root user + klaudiush server config (static) ---
# node:24-slim already defines a `node` user at uid 1000 — remove it before
# creating `sybra` so we can reuse uid 1000 (a common bind-mount convention).
# Server-tuned klaudiush config: drop -S (no GPG key on server),
# keep -s sign-off + conventional commit rules. XDG path so klaudiush
# doctor does not warn about legacy ~/.klaudiush/ location.
RUN userdel -r node 2>/dev/null || true \
    && groupadd -g "${SYBRA_GID}" sybra \
    && useradd -l -m -u "${SYBRA_UID}" -g "${SYBRA_GID}" -s /bin/bash -d /home/sybra sybra \
    && mkdir -p /home/sybra/.config/klaudiush \
    && printf '%s\n' \
        '[validators.git.commit]' \
        'enabled = true' \
        'severity = "error"' \
        'required_flags = ["-s"]' \
        'check_staging_area = true' \
        'enable_message_validation = true' \
        '' \
        '[validators.git.commit.message]' \
        'title_max_length = 50' \
        'body_max_line_length = 72' \
        'check_conventional_commits = true' \
        'require_scope = true' \
        'block_infra_scope_misuse = true' \
        'block_pr_references = true' \
        'block_ai_attribution = true' \
        '' \
        '[validators.git.no_verify]' \
        'enabled = true' \
        'severity = "error"' \
        '' \
        '[overrides.entries.FILE005]' \
        'disabled = true' \
        'reason = "YAML frontmatter false positive in task files"' \
        > /home/sybra/.config/klaudiush/config.toml \
    && chown -R "${SYBRA_UID}:${SYBRA_GID}" /home/sybra

# --- Layer F+G: thin, per-commit layers ---
COPY --from=go-builder /bin/sybra-server /usr/local/bin/sybra-server
COPY --from=go-builder /bin/sybra-cli /usr/local/bin/sybra-cli
COPY --from=frontend-builder /app/frontend/dist-web /app/web
COPY internal /app/src/internal
COPY orchestrator /app/src/orchestrator
COPY go.mod go.sum README.md CLAUDE.md /app/src/

ENV SYBRA_PORT=8080
ENV SYBRA_STATIC_DIR=/app/web
ENV HOME=/home/sybra

USER sybra
WORKDIR /home/sybra

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -sf "http://localhost:${SYBRA_PORT}/health" || exit 1

# Mounts expected (host dirs must be chowned to uid:gid 1000:1000):
#   ~/.sybra  → /home/sybra/.sybra  (task store, config, projects)
#   ~/.claude   → /home/sybra/.claude   (claude CLI config + session, must contain settings.json with klaudiush hooks)
#   ~/.codex    → /home/sybra/.codex    (codex CLI config, must contain config.toml + hooks.json with klaudiush hooks)
ENTRYPOINT ["/usr/local/bin/sybra-server"]
