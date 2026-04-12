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
FROM node:24-slim AS runtime

RUN npm install -g @anthropic-ai/claude-code \
    && rm -rf /root/.npm

COPY --from=go-builder /bin/synapse-server /usr/local/bin/synapse-server
COPY --from=frontend-builder /app/frontend/dist-web /app/web

ENV SYNAPSE_PORT=8080
ENV SYNAPSE_STATIC_DIR=/app/web

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD node -e "require('http').get('http://localhost:'+process.env.SYNAPSE_PORT+'/health',r=>{process.exit(r.statusCode===200?0:1)}).on('error',()=>process.exit(1))"

# Mounts expected:
#   ~/.synapse  → /root/.synapse  (task store, config, projects)
#   ~/.claude   → /root/.claude   (claude CLI config + session)
ENTRYPOINT ["/usr/local/bin/synapse-server"]
