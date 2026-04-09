import { test, expect, type Page } from '@playwright/test'
import { copyFile, unlink, readFile, mkdir } from 'node:fs/promises'
import { existsSync } from 'node:fs'
import { join } from 'node:path'
import { homedir } from 'node:os'

const SYNAPSE_HOME = process.env.SYNAPSE_HOME ?? join(homedir(), '.synapse')
const WORKFLOWS_DIR = join(SYNAPSE_HOME, 'workflows')
const FIXTURE_ID = 'wf-editor-e2e'
const FIXTURE_DEST = join(WORKFLOWS_DIR, `${FIXTURE_ID}.yaml`)

async function ensureFixture() {
  if (!existsSync(WORKFLOWS_DIR)) {
    await mkdir(WORKFLOWS_DIR, { recursive: true })
  }
  const src = join(import.meta.dirname, 'fixtures', 'wf-editor-e2e.yaml')
  await copyFile(src, FIXTURE_DEST)
}

async function removeFixture() {
  if (existsSync(FIXTURE_DEST)) {
    await unlink(FIXTURE_DEST)
  }
}

async function openWorkflowEditor(page: Page) {
  await page.goto('/')
  await page.locator('[data-part="trigger"]', { hasText: /Workflows/ }).click()
  // Wait for the fixture card to appear and click it.
  const card = page.getByRole('button', { name: /E2E Editor Fixture/ })
  await expect(card).toBeVisible({ timeout: 10_000 })
  await card.click()
  // Detail header should render the workflow name.
  await expect(
    page.locator('h2', { hasText: 'E2E Editor Fixture' }),
  ).toBeVisible()
}

test.beforeAll(async () => {
  await ensureFixture()
})

test.afterAll(async () => {
  await removeFixture()
})

test.describe('Workflow editor — list page', () => {
  test('list card shows trigger event and condition count', async ({
    page,
  }) => {
    await page.goto('/')
    await page
      .locator('[data-part="trigger"]', { hasText: /Workflows/ })
      .click()

    const card = page.getByRole('button', { name: /E2E Editor Fixture/ })
    await expect(card).toBeVisible({ timeout: 10_000 })
    await expect(card).toContainText('trigger: task.created')
    await expect(card).toContainText('1 cond')
    await expect(card).toContainText('1 steps')
  })
})

test.describe('Workflow editor — trigger panel', () => {
  test('renders event and existing condition', async ({ page }) => {
    await openWorkflowEditor(page)

    // Trigger section header + summary
    const triggerSection = page.locator('div', {
      has: page.locator('span', { hasText: 'Trigger' }),
    }).first()
    await expect(triggerSection).toContainText('task.created')
    await expect(triggerSection).toContainText('1 condition')

    // Condition row fields should be present with seeded values
    await expect(page.locator('input[value="task.tags"]')).toBeVisible()
    await expect(page.locator('input[value="skip"]')).toBeVisible()
  })

  test('can add a new trigger condition', async ({ page }) => {
    await openWorkflowEditor(page)

    const addBtn = page.getByRole('button', { name: /\+ Add condition/ })
    await addBtn.click()

    // After adding, summary should reflect 2 conditions
    await expect(
      page.locator('span', { hasText: /2 conditions/ }),
    ).toBeVisible()

    // Unsaved badge should appear.
    await expect(page.locator('span', { hasText: 'unsaved' })).toBeVisible()
  })
})

test.describe('Workflow editor — add step + transitions', () => {
  test('clicking + Add step creates a new step and opens the config panel', async ({
    page,
  }) => {
    await openWorkflowEditor(page)

    // Panel should not be visible yet (no step selected).
    await expect(
      page.locator('h3', { hasText: 'Step Config' }),
    ).not.toBeVisible()

    await page.getByRole('button', { name: /\+ Add step/ }).click()

    // Config panel opens with the seeded default name.
    await expect(
      page.locator('h3', { hasText: 'Step Config' }),
    ).toBeVisible()
    await expect(page.locator('input[value="New step"]')).toBeVisible()

    // Transitions section is visible and empty by default.
    await expect(
      page.locator('span', { hasText: /^Transitions$/ }),
    ).toBeVisible()
    await expect(
      page.getByText('No transitions — step ends the workflow'),
    ).toBeVisible()

    // Unsaved badge appears after mutation.
    await expect(page.locator('span', { hasText: 'unsaved' })).toBeVisible()
  })

  test('can add a transition targeting an existing step', async ({ page }) => {
    await openWorkflowEditor(page)

    // Select the existing step by clicking its graph node.
    await page.locator('.svelte-flow__node-stepNode').first().click()
    await expect(
      page.locator('h3', { hasText: 'Step Config' }),
    ).toBeVisible()

    // Transitions section → + Add
    await page
      .locator('div', {
        has: page.locator('span', { hasText: /^Transitions$/ }),
      })
      .getByRole('button', { name: '+ Add' })
      .click()

    // A new transition row with goto dropdown defaulting to <end workflow>.
    const gotoSelect = page.locator('select').filter({ hasText: /end workflow/ })
    await expect(gotoSelect).toBeVisible()

    // Toggle conditional (when) checkbox.
    const whenCheckbox = page.getByRole('checkbox', { name: /conditional/ })
    await whenCheckbox.check()
    await expect(whenCheckbox).toBeChecked()

    await expect(page.locator('span', { hasText: 'unsaved' })).toBeVisible()
  })
})

test.describe('Workflow editor — save round-trip', () => {
  test('add step + save persists to disk', async ({ page }) => {
    await openWorkflowEditor(page)

    await page.getByRole('button', { name: /\+ Add step/ }).click()

    // Change the name to something identifiable.
    const nameInput = page.locator('input[value="New step"]')
    await nameInput.fill('e2e-added-step')
    await nameInput.blur()

    // Save via the header button.
    await page.getByRole('button', { name: /^Save$/ }).click()

    // Unsaved badge should clear.
    await expect(
      page.locator('span', { hasText: 'unsaved' }),
    ).not.toBeVisible({ timeout: 5_000 })

    // Verify the YAML on disk now contains the new step.
    const yaml = await readFile(FIXTURE_DEST, 'utf8')
    expect(yaml).toContain('e2e-added-step')
    // Original step still there.
    expect(yaml).toContain('first-step')
  })
})
