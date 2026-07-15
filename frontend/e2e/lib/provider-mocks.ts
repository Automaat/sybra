import type { Page } from '@playwright/test'

const HEALTHY = [
  {
    provider: 'claude',
    healthy: true,
    reason: 'ok',
    lastCheck: '2026-04-16T12:00:00Z',
  },
  {
    provider: 'codex',
    healthy: true,
    reason: 'ok',
    lastCheck: '2026-04-16T12:00:00Z',
  },
]

/**
 * Mocks provider-health endpoints so the CLI-probe banners
 * (`claude unavailable — probe_error`) never appear in screenshots.
 * Apply in `beforeEach` before any navigation.
 */
export async function mockProviderHealth(page: Page) {
  await page.route('**/api/IntegrationService/GetProviderHealth', (route) =>
    route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify(HEALTHY),
    }),
  )

  await page.route('**/api/IntegrationService/ProviderHealthEnabled', (route) =>
    route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify(true),
    }),
  )
}
