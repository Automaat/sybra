/// <reference types="vitest/config" />

import { defineConfig, loadEnv } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'
import tailwindcss from '@tailwindcss/vite'
import { fileURLToPath, URL } from 'url'

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
