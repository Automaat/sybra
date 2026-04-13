# Stage 1: Build web frontend
FROM node:24-slim AS frontend-builder
WORKDIR /app/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build:web

# Stage 2: Build synapse-server binary
FROM golang:1.26.2-bookworm AS go-builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/synapse-server ./cmd/synapse-server

# Stage 3: Runtime — node:24-slim for claude CLI (Node.js-based)
#
# Layer ordering targets pull-time on re-deploys: the heavy, version-pinned
# tool layers sit above the thin synapse-binary + web-assets layers. Bumping
# synapse invalidates only the last two COPY layers (~20MB); bumping a tool
# ARG invalidates just that tool's layer. Pinned versions keep layer digests
# stable across rebuilds (unpinned `apt-get` / `npm install` would otherwise
# regenerate the blob on every build even when inputs are unchanged).
FROM node:24-slim AS runtime

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
# renovate: datasource=github-releases depName=smykla-skalski/klaudiush
ARG KLAUDIUSH_VERSION=v1.32.0
RUN curl -sSfL https://klaudiu.sh/install.sh \
         | sh -s -- -b /usr/local/bin -v "${KLAUDIUSH_VERSION}"

# --- Layer C: node CLIs (claude code + codex), pinned for cache stability ---
# renovate: datasource=npm depName=@anthropic-ai/claude-code
ARG CLAUDE_CODE_VERSION=2.1.104
# renovate: datasource=npm depName=@openai/codex
ARG CODEX_VERSION=0.120.0
RUN npm install -g \
        "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
        "@openai/codex@${CODEX_VERSION}" \
    && rm -rf /root/.npm

# --- Layer D: klaudiush server config (static) ---
# Server-tuned klaudiush config: drop -S (no GPG key on server),
# keep -s sign-off + conventional commit rules. XDG path so klaudiush
# doctor does not warn about legacy ~/.klaudiush/ location.
RUN mkdir -p /root/.config/klaudiush && printf '%s\n' \
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
    > /root/.config/klaudiush/config.toml

# --- Layer E+F: thin, per-commit layers ---
COPY --from=go-builder /bin/synapse-server /usr/local/bin/synapse-server
COPY --from=frontend-builder /app/frontend/dist-web /app/web

ENV SYNAPSE_PORT=8080
ENV SYNAPSE_STATIC_DIR=/app/web

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD node -e "require('http').get('http://localhost:'+process.env.SYNAPSE_PORT+'/health',r=>{process.exit(r.statusCode===200?0:1)}).on('error',()=>process.exit(1))"

# Mounts expected:
#   ~/.synapse  → /root/.synapse  (task store, config, projects)
#   ~/.claude   → /root/.claude   (claude CLI config + session, must contain settings.json with klaudiush hooks)
#   ~/.codex    → /root/.codex    (codex CLI config, must contain config.toml + hooks.json with klaudiush hooks)
ENTRYPOINT ["/usr/local/bin/synapse-server"]
