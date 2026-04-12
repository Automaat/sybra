import { defineConfig } from 'vitest/config'
import { svelte } from '@sveltejs/vite-plugin-svelte'
import tailwindcss from '@tailwindcss/vite'
import { fileURLToPath, URL } from 'url'

const mode = process.env.VITE_MODE ?? 'desktop'
const outDir = mode === 'web' ? 'dist-web' : 'dist'

export default defineConfig({
  plugins: [
    tailwindcss(),
    svelte(),
  ],
  build: {
    outDir,
  },
  define: {
    'import.meta.env.VITE_MODE': JSON.stringify(mode),
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
    coverage: {
      provider: 'v8',
      reporter: ['text', 'json'],
      include: ['src/**/*.ts', 'src/**/*.svelte'],
      exclude: ['src/main.ts'],
    },
  },
})
