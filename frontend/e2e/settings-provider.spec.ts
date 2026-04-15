import { test, expect, type Page } from '@playwright/test'
import { selectedProviders } from './lib/providers.js'

async function goToSettings(page: Page) {
  await page.goto('/')
  await page.locator('[data-part="trigger"]', { hasText: /Settings/ }).click()
  await expect(page.locator('h1', { hasText: 'Settings' })).toBeVisible()
  await expect(page.locator('#agent-provider')).toBeVisible()
}

async function currentAgentSettings(page: Page) {
  return {
    provider: await page.locator('#agent-provider').inputValue(),
    model: await page.locator('#agent-model').inputValue(),
    mode: await page.locator('#agent-mode').inputValue(),
  }
}

async function saveSettings(page: Page) {
  const saveButton = page.getByRole('button', { name: 'Save' })
  if (await saveButton.isDisabled()) {
    return
  }
  await saveButton.click()
  await expect(page.getByText('Settings saved')).toBeVisible()
}

async function restoreAgentSettings(page: Page, original: { provider: string; model: string; mode: string }) {
  if ((await page.locator('#agent-provider').count()) === 0) {
    await goToSettings(page)
  }
  await page.locator('#agent-provider').selectOption(original.provider)
  await page.locator('#agent-model').selectOption(original.model)
  await page.locator('#agent-mode').selectOption(original.mode)
  await saveSettings(page)
}

test.describe.configure({ mode: 'serial' })

test.describe('Settings provider matrix', () => {
  for (const spec of selectedProviders()) {
    test(`switches model options for ${spec.provider}`, async ({ page }) => {
      await goToSettings(page)
      const original = await currentAgentSettings(page)

      try {
        await page.locator('#agent-provider').selectOption(spec.provider)

        const optionTexts = await page.locator('#agent-model option').allTextContents()
        expect(optionTexts).toEqual(spec.expectedOptions)
        await expect(page.locator('#agent-model option').first()).toHaveText(spec.modelLabel)
      } finally {
        await restoreAgentSettings(page, original)
      }
    })

    test(`persists ${spec.provider} after save + reload`, async ({ page }) => {
      await goToSettings(page)
      const original = await currentAgentSettings(page)

      try {
        await page.locator('#agent-provider').selectOption(spec.provider)
        await page.locator('#agent-model').selectOption('')
        await saveSettings(page)

        await page.reload()
        await goToSettings(page)
        await expect(page.locator('#agent-provider')).toHaveValue(spec.provider)
        await expect(page.locator('#agent-model option').first()).toHaveText(spec.modelLabel)
      } finally {
        await restoreAgentSettings(page, original)
      }
    })
  }

  test('provider switch resets incompatible model to default option', async ({ page }) => {
    await goToSettings(page)
    const original = await currentAgentSettings(page)

    try {
      await page.locator('#agent-provider').selectOption('claude')
      await page.locator('#agent-model').selectOption('haiku')
      await saveSettings(page)

      await page.locator('#agent-provider').selectOption('codex')
      await expect(page.locator('#agent-model')).toHaveValue('')
      await expect(page.locator('#agent-model option').first()).toHaveText('Default (gpt-5.4)')
    } finally {
      await restoreAgentSettings(page, original)
    }
  })
})
