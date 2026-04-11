import { defineConfig } from '@playwright/test'

export default defineConfig({
  testDir: './e2e',
  timeout: 10_000,
  retries: process.env.CI ? 2 : 0,
  reporter: process.env.CI ? 'github' : 'list',
  use: {
    baseURL: 'http://localhost:34115',
    screenshot: 'only-on-failure',
    trace: 'on-first-retry',
  },
  projects: [
    {
      name: 'chromium',
      use: { browserName: 'chromium' },
    },
  ],
  webServer: {
    command: 'cd .. && wails dev',
    url: 'http://localhost:34115',
    // Always start a fresh instance in CI; reuse an existing dev server locally.
    reuseExistingServer: !process.env.CI,
    timeout: 150_000,
  },
})
