/// <reference types="vitest/config" />

import { defineConfig, loadEnv } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'
import tailwindcss from '@tailwindcss/vite'
import { fileURLToPath, URL } from 'url'
import os from 'node:os'

// Mirrors internal/testutil/loadscale + e2eTimeoutScaleResolve
// (internal/sybra/e2e_workflow_test.go) so vitest's default 5s testTimeout
// survives the same conditions the Go e2e suite scales for: CI runners and
// a locally-loaded host (e.g. a fleet of concurrent agents).
const vitestTimeoutScaleCeiling = 20
const vitestCITimeoutScaleFloor = 12
const vitestBaseTestTimeoutMs = 5000

// Matches strconv.ParseInt(v, 10, 64): the trimmed string must be entirely a
// base-10 integer (optional leading sign, digits only) — no partial-prefix
// parses like "3x" get accepted the way Number.parseInt would accept them.
const strconvBase10IntPattern = /^[+-]?\d+$/

function vitestTimeoutScale(): number {
  const override = process.env.SYBRA_E2E_TIMEOUT_SCALE?.trim()
  if (override && strconvBase10IntPattern.test(override)) {
    const n = Number.parseInt(override, 10)
    if (Number.isFinite(n) && n > 0) return n
  }
  const cpus = os.cpus().length || 1
  const loadPerCPU = os.loadavg()[0] / cpus
  const factor = loadPerCPU > 1
    ? Math.min(Math.ceil(loadPerCPU), vitestTimeoutScaleCeiling)
    : 1
  if (!process.env.CI && !process.env.GITHUB_ACTIONS) {
    return factor
  }
  return Math.min(Math.max(vitestCITimeoutScaleFloor * factor, vitestCITimeoutScaleFloor), vitestTimeoutScaleCeiling)
}

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '')
  const viteMode = env.VITE_MODE ?? 'desktop'
  const outDir = viteMode === 'web' ? 'dist-web' : 'dist'

  // Web-mode dev server proxies the HTTP API (/api) and the SSE event stream
  // (/events) to a running sybra-server so `npm run dev` gives HMR against a
  // real backend without CORS. Target overridable via SYBRA_PROXY_TARGET.
  const proxyTarget = env.SYBRA_PROXY_TARGET || 'http://localhost:8080'

  return {
    server: {
      proxy: {
        '/api': { target: proxyTarget, changeOrigin: true },
        // SSE: disable proxy buffering so events flush to the browser live.
        '/events': { target: proxyTarget, changeOrigin: true },
        // Names the board and, on a loopback hop, carries its token.
        '/runtime-config.js': { target: proxyTarget, changeOrigin: true },
      },
    },
    plugins: [
      tailwindcss(),
      svelte(),
    ],
    build: {
      outDir,
      target: 'esnext',
      chunkSizeWarningLimit: 1000,
      rollupOptions: {
        output: {
          manualChunks(id) {
            // highlight.js + marked are pulled in eagerly by the markdown
            // renderer (initial paint path), so a stable vendor chunk improves
            // long-term caching. @xyflow is intentionally NOT forced here: it
            // is only reached through the lazily-imported WorkflowDetail route,
            // and a manual chunk would hoist it back into the initial graph
            // (eager modulepreload). Letting Rollup split it keeps it inside
            // the WorkflowDetail async chunk.
            if (id.includes('highlight.js')) return 'vendor-highlight'
            if (id.includes('/marked')) return 'vendor-markdown'
          },
        },
      },
    },
    optimizeDeps: {
      include: ['highlight.js', 'marked', 'marked-highlight', 'js-yaml', 'snarkdown'],
    },
    define: {
      'import.meta.env.VITE_MODE': JSON.stringify(viteMode),
    },
    resolve: {
      conditions: ['browser'],
      alias: {
        '$lib': fileURLToPath(new URL('./src/lib', import.meta.url)),
      },
    },
    test: {
      environment: 'jsdom',
      include: ['src/**/*.test.ts'],
      exclude: ['e2e/**', 'node_modules/**'],
      setupFiles: ['./src/test-setup.ts'],
      testTimeout: vitestBaseTestTimeoutMs * vitestTimeoutScale(),
      coverage: {
        provider: 'v8',
        reporter: ['text', 'json'],
        include: ['src/**/*.ts', 'src/**/*.svelte'],
        exclude: ['src/main.ts'],
        thresholds: {
          statements: 65,
          branches: 55,
          functions: 58,
          lines: 65,
        },
      },
    },
  }
})
