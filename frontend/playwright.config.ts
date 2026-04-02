import { defineConfig } from '@playwright/test'

export default defineConfig({
  testDir: './e2e',
  timeout: 15_000,
  retries: 0,
  reporter: process.env.CI ? 'github' : 'list',
  use: {
    baseURL: 'http://localhost:34115',
    screenshot: 'on',
    trace: 'on',
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
    reuseExistingServer: true,
    timeout: 60_000,
  },
})
