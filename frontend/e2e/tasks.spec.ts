import { test, expect, type Page } from '@playwright/test'
import { readdir, unlink } from 'node:fs/promises'
import { join, dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const TASKS_DIR = resolve(dirname(fileURLToPath(import.meta.url)), '../../tasks')

async function cleanupCreatedTasks() {
  const files = await readdir(TASKS_DIR)
  for (const f of files) {
    if (!f.startsWith('sample-') && f.endsWith('.md')) {
      await unlink(join(TASKS_DIR, f))
    }
  }
}

async function goToTaskList(page: Page) {
  await page.goto('/')
  await page.locator('[data-part="trigger"]', { hasText: 'Tasks' }).click()
  await page.waitForSelector('button:has(h3), :text("No tasks")', { timeout: 10_000 })
}

async function waitForTasks(page: Page) {
  await page.waitForSelector('button:has(h3), :text("No tasks")', { timeout: 10_000 })
}

// Click a SegmentedControl item by label text within a scoped container
async function clickSegment(page: Page, scope: 'main' | 'dialog', label: string) {
  const container = scope === 'dialog' ? page.getByRole('dialog') : page.getByRole('main')
  await container.locator(`[data-part="item-text"]`, { hasText: label }).first().click()
}

test.afterAll(async () => {
  await cleanupCreatedTasks()
})

test.describe('Task List', () => {
  test('displays sample tasks on load', async ({ page }) => {
    await goToTaskList(page)

    await expect(page.getByText('Implement auth middleware')).toBeVisible()
    await expect(page.getByText('Write API integration tests')).toBeVisible()
    await expect(page.getByText('Design database migration strategy')).toBeVisible()
  })

  test('shows status badges with correct labels', async ({ page }) => {
    await goToTaskList(page)

    await expect(page.getByText('Todo').first()).toBeVisible()
    await expect(page.getByText('In Progress').first()).toBeVisible()
    await expect(page.getByText('Blocked').first()).toBeVisible()
  })

  test('shows app bar with Tasks title and New Task button', async ({ page }) => {
    await goToTaskList(page)

    await expect(page.locator('h2', { hasText: 'Tasks' })).toBeVisible()
    await expect(page.getByText('+ New Task')).toBeVisible()
  })

  test('filters tasks by status', async ({ page }) => {
    await goToTaskList(page)

    // Click "Todo" filter
    await clickSegment(page, 'main', 'Todo')
    await expect(page.getByText('Implement auth middleware')).toBeVisible()
    await expect(page.getByText('Write API integration tests')).not.toBeVisible()
    await expect(page.getByText('Design database migration strategy')).not.toBeVisible()

    // Click "Blocked" filter
    await clickSegment(page, 'main', 'Blocked')
    await expect(page.getByText('Design database migration strategy')).toBeVisible()
    await expect(page.getByText('Implement auth middleware')).not.toBeVisible()

    // Click "All" to reset
    await clickSegment(page, 'main', 'All')
    await expect(page.getByText('Implement auth middleware')).toBeVisible()
    await expect(page.getByText('Write API integration tests')).toBeVisible()
    await expect(page.getByText('Design database migration strategy')).toBeVisible()
  })

  test('shows empty state when filter matches nothing', async ({ page }) => {
    await goToTaskList(page)

    // "Done" filter — no sample tasks have done status
    await clickSegment(page, 'main', 'Done')

    await expect(page.getByText('No tasks')).toBeVisible()
    await expect(page.getByText('Create a task to get started')).toBeVisible()
  })
})

test.describe('Task Detail', () => {
  test('navigates to task detail on card click', async ({ page }) => {
    await goToTaskList(page)

    await page.getByText('Implement auth middleware').click()

    await expect(page.locator('h1', { hasText: 'Implement auth middleware' })).toBeVisible()
    await expect(page.getByText('Back to tasks')).toBeVisible()
    await expect(page.locator('h2', { hasText: 'Task Detail' })).toBeVisible()
  })

  test('shows task metadata', async ({ page }) => {
    await goToTaskList(page)

    await page.getByText('Implement auth middleware').click()
    await expect(page.locator('h1', { hasText: 'Implement auth middleware' })).toBeVisible()

    const main = page.getByRole('main')

    // Agent mode
    await expect(main.getByText('Agent Mode', { exact: true })).toBeVisible()
    await expect(main.getByText('headless').first()).toBeVisible()

    // Tags
    await expect(main.getByText('Tags')).toBeVisible()
    await expect(main.getByText('backend').first()).toBeVisible()
    await expect(main.getByText('auth', { exact: true })).toBeVisible()

    // Body
    await expect(main.getByText('Add JWT middleware to the API router.')).toBeVisible()

    // Timestamps
    await expect(main.getByText(/Created:/)).toBeVisible()
    await expect(main.getByText(/Updated:/)).toBeVisible()
  })

  test('navigates back to list', async ({ page }) => {
    await goToTaskList(page)

    await page.getByText('Implement auth middleware').click()
    await expect(page.locator('h1', { hasText: 'Implement auth middleware' })).toBeVisible()

    await page.getByText('Back to tasks').click()

    await expect(page.locator('h2', { hasText: 'Tasks' })).toBeVisible()
    await expect(page.getByText('Implement auth middleware')).toBeVisible()
  })

  test('changes task status via segmented control', async ({ page }) => {
    await goToTaskList(page)

    await page.getByText('Implement auth middleware').click()
    await expect(page.locator('h1', { hasText: 'Implement auth middleware' })).toBeVisible()

    // Change status to In Progress
    await clickSegment(page, 'main', 'In Progress')

    // Wait for backend update
    await page.waitForTimeout(500)

    // Go back and verify the list reflects the change
    await page.getByText('Back to tasks').click()
    await waitForTasks(page)

    const card = page.locator('button', { hasText: 'Implement auth middleware' })
    await expect(card.getByText('In Progress')).toBeVisible()

    // Restore original status
    await card.click()
    await expect(page.locator('h1', { hasText: 'Implement auth middleware' })).toBeVisible()
    await clickSegment(page, 'main', 'Todo')
  })
})

test.describe('Create Task', () => {
  test('opens and closes create dialog', async ({ page }) => {
    await goToTaskList(page)

    await page.getByText('+ New Task').click()
    await expect(page.getByRole('dialog')).toBeVisible()
    await expect(page.getByPlaceholder('Task title...')).toBeVisible()

    // Close via Cancel
    await page.getByText('Cancel').click()
    await expect(page.getByPlaceholder('Task title...')).not.toBeVisible()
  })

  test('creates a new task and navigates to detail', async ({ page }) => {
    await goToTaskList(page)

    await page.getByText('+ New Task').click()
    await expect(page.getByRole('dialog')).toBeVisible()

    // Fill form
    await page.getByPlaceholder('Task title...').fill('E2E Test Task')
    await page.getByPlaceholder('Task description (markdown)...').fill('Created by Playwright e2e test')

    // Switch to interactive mode
    await clickSegment(page, 'dialog', 'Interactive')

    // Submit
    await page.getByRole('button', { name: 'Create' }).click()

    // Should navigate to detail view of the new task
    await expect(page.locator('h1', { hasText: 'E2E Test Task' })).toBeVisible({ timeout: 5_000 })
    await expect(page.getByText('Created by Playwright e2e test')).toBeVisible()

    // Agent mode should show interactive in the detail metadata
    const main = page.getByRole('main')
    await expect(main.getByText('Agent Mode').locator('..').getByText('interactive')).toBeVisible()

    // Go back — new task should appear in list
    await page.getByText('Back to tasks').click()
    await waitForTasks(page)
    await expect(page.getByText('E2E Test Task')).toBeVisible()
  })

  test('create button is disabled without title', async ({ page }) => {
    await goToTaskList(page)

    await page.getByText('+ New Task').click()
    await expect(page.getByPlaceholder('Task title...')).toBeVisible()

    const createBtn = page.getByRole('button', { name: 'Create' })
    await expect(createBtn).toBeDisabled()

    await page.getByPlaceholder('Task title...').fill('Test')
    await expect(createBtn).toBeEnabled()

    await page.getByPlaceholder('Task title...').fill('')
    await expect(createBtn).toBeDisabled()
  })
})

test.describe('Navigation Rail', () => {
  test('tasks nav trigger is visible', async ({ page }) => {
    await page.goto('/')
    await expect(page.getByText('Tasks', { exact: true }).first()).toBeVisible()
  })

  test('clicking tasks nav returns to task list from detail', async ({ page }) => {
    await goToTaskList(page)

    await page.getByText('Implement auth middleware').click()
    await expect(page.locator('h1', { hasText: 'Implement auth middleware' })).toBeVisible()

    const navTrigger = page.locator('[data-part="trigger"]', { hasText: 'Tasks' })
    await navTrigger.click()

    await expect(page.locator('h2', { hasText: 'Tasks' })).toBeVisible()
  })
})
