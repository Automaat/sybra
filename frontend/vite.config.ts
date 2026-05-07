import { defineConfig } from 'vitest/config'
import { loadEnv } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'
import tailwindcss from '@tailwindcss/vite'
import { fileURLToPath, URL } from 'url'

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '')
  const viteMode = env.VITE_MODE ?? 'desktop'
  const outDir = viteMode === 'web' ? 'dist-web' : 'dist'

  return {
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
            if (id.includes('@xyflow')) return 'vendor-flow'
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
      },
    },
  }
})
