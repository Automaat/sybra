/**
 * Provider-aware smoke tests that verify core pages render correctly for each
 * configured provider, and that the provider baked into config.yaml is
 * correctly reflected by the Settings UI.
 *
 * These tests are intentionally lightweight — they catch regressions caused by
 * provider-specific configuration rather than exercising the full settings
 * workflow (which lives in settings-provider.spec.ts).
 */
import { test, expect, type Page } from '@playwright/test'
import { providerMatrix, selectedProviders } from './lib/providers.js'

async function goToSettings(page: Page) {
  await page.goto('/')
  await page.locator('[data-part="trigger"]', { hasText: /Settings/ }).click()
  await expect(page.locator('h1', { hasText: 'Settings' })).toBeVisible()
  await expect(page.locator('#agent-provider')).toBeVisible()
}

test.describe.configure({ mode: 'serial' })

// ---------------------------------------------------------------------------
// Smoke: core pages render for each provider in the matrix.
// When SYNAPSE_E2E_PROVIDER is set (CI), only the matching spec runs.
// ---------------------------------------------------------------------------
test.describe('Provider smoke: board', () => {
  for (const spec of selectedProviders()) {
    test(`board loads [${spec.provider}]`, async ({ page }) => {
      await page.goto('/')
      await page.locator('[data-part="trigger"]', { hasText: /Board/ }).click()
      await page.waitForSelector('button:has(h3), :text("No tasks")', { timeout: 10_000 })
      await expect(page.getByRole('heading', { name: 'Todo' })).toBeVisible()
      await expect(page.getByRole('heading', { name: 'In Progress' })).toBeVisible()
    })

    test(`settings page loads [${spec.provider}]`, async ({ page }) => {
      await goToSettings(page)
      await expect(page.locator('#agent-provider')).toBeVisible()
      await expect(page.locator('#agent-model')).toBeVisible()
      await expect(page.locator('#agent-mode')).toBeVisible()
      await expect(page.locator('#agent-concurrency')).toBeVisible()
    })
  }
})

// ---------------------------------------------------------------------------
// Provider config: verify the provider written to config.yaml before the test
// run is reflected in the Settings UI.  Only meaningful in CI where
// SYNAPSE_E2E_PROVIDER is set and config.yaml is pre-seeded.
// ---------------------------------------------------------------------------
test.describe('Provider config reflected in Settings', () => {
  test('active provider matches SYNAPSE_E2E_PROVIDER', async ({ page }) => {
    const envProvider = process.env.SYNAPSE_E2E_PROVIDER?.trim()
    test.skip(!envProvider, 'SYNAPSE_E2E_PROVIDER not set — skipped on local runs')

    const spec = providerMatrix.find((s) => s.provider === envProvider)
    test.skip(!spec, `Unknown provider "${envProvider}"`)

    await goToSettings(page)
    await expect(page.locator('#agent-provider')).toHaveValue(spec!.provider)
    await expect(page.locator('#agent-model option').first()).toHaveText(spec!.modelLabel)
  })

  test('model options match SYNAPSE_E2E_PROVIDER', async ({ page }) => {
    const envProvider = process.env.SYNAPSE_E2E_PROVIDER?.trim()
    test.skip(!envProvider, 'SYNAPSE_E2E_PROVIDER not set — skipped on local runs')

    const spec = providerMatrix.find((s) => s.provider === envProvider)
    test.skip(!spec, `Unknown provider "${envProvider}"`)

    await goToSettings(page)
    const optionTexts = await page.locator('#agent-model option').allTextContents()
    expect(optionTexts).toEqual(spec!.expectedOptions)
  })
})
