import { defineConfig } from 'vitest/config'
import { svelte } from '@sveltejs/vite-plugin-svelte'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [
    tailwindcss(),
    svelte(),
  ],
  resolve: {
    conditions: ['browser'],
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
