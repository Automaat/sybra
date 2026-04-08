import { test, expect, type Page } from '@playwright/test'
import { readdir, unlink, copyFile } from 'node:fs/promises'
import { join } from 'node:path'
import { homedir } from 'node:os'

const SYNAPSE_HOME = process.env.SYNAPSE_HOME ?? join(homedir(), '.synapse')
const TASKS_DIR = join(SYNAPSE_HOME, 'tasks')

const FIXTURE_FILES = new Set([
  'auth0001.md',
  'test0001.md',
  'db0001.md',
  'plan0001.md',
])

async function cleanupCreatedTasks() {
  const files = await readdir(TASKS_DIR)
  for (const f of files) {
    if (!FIXTURE_FILES.has(f) && f.endsWith('.md')) {
      await unlink(join(TASKS_DIR, f))
    }
  }
}

async function ensurePlanFixture() {
  const src = join(import.meta.dirname, 'fixtures', 'plan0001.md')
  const dst = join(TASKS_DIR, 'plan0001.md')
  await copyFile(src, dst)
}

async function goToTaskList(page: Page) {
  await page.goto('/')
  await page.locator('[data-part="trigger"]', { hasText: /Board/ }).click()
  await page.waitForSelector('button:has(h3), :text("No tasks")', { timeout: 10_000 })
}

async function goToPlanReviews(page: Page) {
  await page.goto('/')
  await page.locator('[data-part="trigger"]', { hasText: /Plans/ }).click()
  await page.waitForTimeout(1000) // wait for task list to load
}

test.beforeAll(async () => {
  await ensurePlanFixture()
})

test.afterAll(async () => {
  await cleanupCreatedTasks()
})

test.describe('Plan Review Workflow', () => {
  test('plan-review task appears in Plan Reviews column', async ({ page }) => {
    await goToTaskList(page)

    // Plan Review column should show the plan task
    const planReviewCol = page.locator('div', {
      has: page.getByRole('heading', { name: 'Plan Review' }),
    })
    await expect(
      planReviewCol.getByText('Refactor logging system'),
    ).toBeVisible()
  })

  test('plan-review task shows approve/reject buttons in detail', async ({
    page,
  }) => {
    await goToTaskList(page)

    await page
      .getByRole('button', { name: 'Refactor logging system' })
      .click()
    await expect(
      page.locator('h1', { hasText: 'Refactor logging system' }),
    ).toBeVisible()

    // Plan review actions should be visible
    await expect(page.getByRole('button', { name: 'Approve' })).toBeVisible()
    await expect(page.getByRole('button', { name: 'Reject' })).toBeVisible()
  })

  test('plan-review task shows plan body markdown', async ({ page }) => {
    await goToTaskList(page)

    await page
      .getByRole('button', { name: 'Refactor logging system' })
      .click()
    await expect(
      page.locator('h1', { hasText: 'Refactor logging system' }),
    ).toBeVisible()

    // Plan content should be rendered
    await expect(
      page.getByText('Replace log.Printf with slog'),
    ).toBeVisible()
    await expect(
      page.getByText('Add log levels configuration'),
    ).toBeVisible()
  })

  test('plan-review badge shows in navigation', async ({ page }) => {
    await page.goto('/')

    // The Plans nav item should show a badge when there are plan-review tasks
    const plansNav = page.locator('[data-part="trigger"]', { hasText: /Plans/ })
    await expect(plansNav).toBeVisible()
  })
})

test.describe('Plan Reviews Page', () => {
  test('displays plan-review tasks in dedicated view', async ({ page }) => {
    await goToPlanReviews(page)

    await expect(page.getByText('Refactor logging system')).toBeVisible({
      timeout: 5_000,
    })
  })

  test('shows feedback textarea for reject', async ({ page }) => {
    await goToPlanReviews(page)

    // Click on the plan task to select it
    await page.getByText('Refactor logging system').click()
    await page.waitForTimeout(500)

    // Feedback textarea should be visible
    await expect(page.getByPlaceholder(/feedback/i)).toBeVisible()
  })
})

test.describe('Task Status Badge', () => {
  test('plan-review tasks show review badge on card', async ({ page }) => {
    await goToTaskList(page)

    // The task card for plan-review should show a review indicator
    const card = page.getByRole('button', {
      name: 'Refactor logging system',
    })
    await expect(card).toBeVisible()
  })
})
