import { test, expect, type Page } from '@playwright/test'

type ProviderSpec = {
  provider: 'claude' | 'codex'
  modelLabel: string
  expectedOptions: string[]
}

const providerMatrix: ProviderSpec[] = [
  {
    provider: 'claude',
    modelLabel: 'Default (Sonnet)',
    expectedOptions: ['Default (Sonnet)', 'Opus', 'Sonnet', 'Haiku'],
  },
  {
    provider: 'codex',
    modelLabel: 'Default (gpt-5.4)',
    expectedOptions: ['Default (gpt-5.4)', 'GPT-5.4', 'GPT-5.4 Mini', 'GPT-5.3 Codex'],
  },
]

function selectedProviders(): ProviderSpec[] {
  const provider = process.env.SYNAPSE_E2E_PROVIDER?.trim()
  if (!provider) return providerMatrix
  const filtered = providerMatrix.filter((spec) => spec.provider === provider)
  return filtered.length > 0 ? filtered : providerMatrix
}

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
        await expect(page.locator('h1', { hasText: 'Settings' })).toBeVisible()
        await expect(page.locator('#agent-provider')).toHaveValue(spec.provider)
        await expect(page.locator('#agent-model option').first()).toHaveText(spec.modelLabel)
      } finally {
        await restoreAgentSettings(page, original)
      }
    })
  }
})
