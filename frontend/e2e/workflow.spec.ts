import { test, expect, type Page } from '@playwright/test'
import { readFile, writeFile } from 'node:fs/promises'
import { join } from 'node:path'

import { cleanupStrayTasks, isolatedSybraHome } from './lib/sybra-home'

const SYBRA_HOME = isolatedSybraHome()
const TASKS_DIR = join(SYBRA_HOME, 'tasks')

const FIXTURE_FILES = new Set([
  'auth0001.md',
  'test0001.md',
  'db0001.md',
  'plan0001.md',
])

async function cleanupCreatedTasks() {
  await cleanupStrayTasks(SYBRA_HOME, TASKS_DIR, FIXTURE_FILES)
}

async function ensurePlanFixture() {
  const src = join(import.meta.dirname, 'fixtures', 'plan0001.md')
  const dst = join(TASKS_DIR, 'plan0001.md')
  // Rewrite timestamps to now so the monitor does not flag the task as stuck
  const now = new Date().toISOString()
  let content = await readFile(src, 'utf8')
  content = content.replace(/created_at: .+/, `created_at: ${now}`)
  content = content.replace(/updated_at: .+/, `updated_at: ${now}`)
  await writeFile(dst, content)
}

async function goToTaskList(page: Page) {
  await page.goto('/')
  await page.locator('[data-part="trigger"]', { hasText: /Board/ }).click()
  // List is the default view now; switch to the board for these column-based tests.
  await page.getByRole('button', { name: 'Board view' }).click()
  await page.waitForSelector('button:has(h3), :text("No tasks")', {
    timeout: 10_000,
  })
}

async function goToPlanReviews(page: Page) {
  await page.goto('/')
  await page
    .locator('[data-part="trigger"]', { hasText: /Reviews/ })
    .filter({ hasNotText: 'Test' })
    .click()
  await page.waitForSelector('button, :text("No plans")', { timeout: 10_000 })
}

test.beforeAll(async () => {
  await cleanupCreatedTasks()
  await ensurePlanFixture()
})

test.afterAll(async () => {
  await cleanupCreatedTasks()
})

test.describe('Plan Review Workflow', () => {
  test('plan-review task appears in Planning column', async ({ page }) => {
    await goToTaskList(page)

    // Planning column includes plan-review status
    const planningCol = page.locator('div', {
      has: page.locator('h2', { hasText: 'Planning' }),
    })
    await expect(
      planningCol.locator('h3').filter({ hasText: 'Refactor logging system' }),
    ).toBeVisible()
  })

  test('plan-review task shows approve/reject buttons in detail', async ({
    page,
  }) => {
    await goToTaskList(page)

    await page
      .locator('button', { has: page.locator('h3', { hasText: 'Refactor logging system' }) })
      .click()
    await expect(
      page.locator('h1', { hasText: 'Refactor logging system' }),
    ).toBeVisible()

    // Plan review lives in the Plan tab (Overview is the default tab).
    await page.locator('[data-part="item"]', { hasText: 'Plan' }).click()

    // Plan review actions should be visible
    await expect(
      page.getByRole('button', { name: 'Approve Plan' }),
    ).toBeVisible()
    await expect(
      page.getByRole('button', { name: 'Reject Plan' }),
    ).toBeVisible()
  })

  test('plan-review task shows plan body markdown', async ({ page }) => {
    await goToTaskList(page)

    await page
      .locator('button', { has: page.locator('h3', { hasText: 'Refactor logging system' }) })
      .click()
    await expect(
      page.locator('h1', { hasText: 'Refactor logging system' }),
    ).toBeVisible()

    // This fixture's plan lives in the task body, so it renders in the
    // description on the default Overview tab (no tab switch needed).
    await expect(
      page.getByText('Replace log.Printf with slog'),
    ).toBeVisible()
    await expect(
      page.getByText('Add log levels configuration'),
    ).toBeVisible()
  })

  test('reviews nav item is visible', async ({ page }) => {
    await page.goto('/')

    const reviewsNav = page
      .locator('[data-part="trigger"]', { hasText: /Reviews/ })
      .filter({ hasNotText: 'Test' })
    await expect(reviewsNav).toBeVisible()
  })
})

test.describe('Plan Reviews Page', () => {
  test('displays plan-review tasks in dedicated view', async ({ page }) => {
    await goToPlanReviews(page)

    await expect(
      page.locator('span.text-sm.font-medium', { hasText: 'Refactor logging system' }),
    ).toBeVisible({
      timeout: 5_000,
    })
  })

  test('shows feedback textarea for reject', async ({ page }) => {
    await goToPlanReviews(page)

    // Click on the plan task to select it
    await page
      .locator('button', {
        has: page.locator('span.text-sm.font-medium', {
          hasText: 'Refactor logging system',
        }),
      })
      .click()
    await page.waitForTimeout(500)

    // Feedback textarea should be visible
    await expect(
      page.getByPlaceholder(/rejection feedback/i),
    ).toBeVisible()
  })
})

test.describe('Task Status Badge', () => {
  test('plan-review tasks show in board', async ({ page }) => {
    await goToTaskList(page)

    // The task card should be visible in the board
    const card = page.locator('button', {
      has: page.locator('h3', { hasText: 'Refactor logging system' }),
    })
    await expect(card).toBeVisible()
  })
})
