import { test, expect, type Page } from '@playwright/test'
import { readdir, unlink, writeFile } from 'node:fs/promises'
import { join } from 'node:path'
import { homedir } from 'node:os'

const SYNAPSE_HOME = process.env.SYNAPSE_HOME ?? join(homedir(), '.synapse')
const TASKS_DIR = join(SYNAPSE_HOME, 'tasks')

const FIXTURE_FILES = new Set(['auth0001.md', 'test0001.md', 'db0001.md', 'plan0001.md'])

async function cleanupCreatedTasks() {
  const files = await readdir(TASKS_DIR)
  for (const f of files) {
    if (!FIXTURE_FILES.has(f) && f.endsWith('.md')) {
      await unlink(join(TASKS_DIR, f))
    }
  }
}

async function goToTaskList(page: Page) {
  await page.goto('/')
  await page.locator('[data-part="trigger"]', { hasText: /Board/ }).click()
  await page.waitForSelector('button:has(h3), :text("No tasks")', { timeout: 10_000 })
}

async function waitForTasks(page: Page) {
  await page.waitForSelector('button:has(h3), :text("No tasks")', { timeout: 10_000 })
}

async function createExternalTaskFile(title: string): Promise<string> {
  const id = `${Date.now().toString(16).slice(-8)}`
  const content = `---
id: ${id}
title: ${title}
status: todo
agent_mode: headless
allowed_tools: []
tags: [e2e, watcher]
created_at: 2026-04-01T00:00:00Z
updated_at: 2026-04-01T00:00:00Z
---
## Description
Task created externally during Playwright run.
`
  const path = join(TASKS_DIR, `${id}.md`)
  await writeFile(path, content, 'utf8')
  return id
}

// Select a status value on the task detail status dropdown
async function selectStatus(page: Page, value: string) {
  await page.getByTestId('task-status-select').selectOption(value)
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

  test('shows kanban columns (Done hidden by default)', async ({ page }) => {
    await goToTaskList(page)

    await expect(page.getByRole('heading', { name: 'Todo' })).toBeVisible()
    await expect(page.getByRole('heading', { name: 'In Progress' })).toBeVisible()
    await expect(page.getByRole('heading', { name: 'In Review' })).toBeVisible()
    await expect(page.getByRole('heading', { name: 'Done' })).not.toBeVisible()
  })

  test('shows app bar with Tasks title and New Task button', async ({ page }) => {
    await goToTaskList(page)

    await expect(page.locator('h2', { hasText: 'Tasks' })).toBeVisible()
    await expect(page.getByText('+ New Task')).toBeVisible()
  })

  test('displays tasks in correct kanban columns', async ({ page }) => {
    await goToTaskList(page)

    // Todo column contains auth middleware task
    const todoCol = page.locator('div', { has: page.getByRole('heading', { name: 'Todo' }) })
    await expect(todoCol.getByText('Implement auth middleware')).toBeVisible()

    // In Progress column contains API tests task
    const inProgressCol = page.locator('div', { has: page.getByRole('heading', { name: 'In Progress' }) })
    await expect(inProgressCol.getByText('Write API integration tests')).toBeVisible()

    // In Review column contains db migration task
    const inReviewCol = page.locator('div', { has: page.getByRole('heading', { name: 'In Review' }) })
    await expect(inReviewCol.getByText('Design database migration strategy')).toBeVisible()
  })
})

test.describe('Task Detail', () => {
  test('navigates to task detail on card click', async ({ page }) => {
    await goToTaskList(page)

    await page.getByRole('button', { name: 'Implement auth middleware' }).click()

    await expect(page.locator('h1', { hasText: 'Implement auth middleware' })).toBeVisible()
    await expect(page.getByText('Back to tasks')).toBeVisible()
    await expect(page.locator('h2', { hasText: 'Task Detail' })).toBeVisible()
  })

  test('shows task metadata', async ({ page }) => {
    await goToTaskList(page)

    await page.getByRole('button', { name: 'Implement auth middleware' }).click()
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

    await page.getByRole('button', { name: 'Implement auth middleware' }).click()
    await expect(page.locator('h1', { hasText: 'Implement auth middleware' })).toBeVisible()

    await page.getByText('Back to tasks').click()

    await expect(page.locator('h2', { hasText: 'Tasks' })).toBeVisible()
    await expect(page.getByText('Implement auth middleware')).toBeVisible()
  })

  test('changes task status via dropdown', async ({ page }) => {
    await goToTaskList(page)

    await page.getByRole('button', { name: 'Implement auth middleware' }).click()
    await expect(page.locator('h1', { hasText: 'Implement auth middleware' })).toBeVisible()

    // Change status to In Progress
    await selectStatus(page, 'in-progress')

    // Wait for backend update
    await page.waitForTimeout(500)

    // Go back and verify the task moved to In Progress column
    await page.getByText('Back to tasks').click()
    await waitForTasks(page)

    const inProgressCol = page.locator('div', { has: page.getByRole('heading', { name: 'In Progress' }) })
    await expect(inProgressCol.getByRole('heading', { name: 'Implement auth middleware' })).toBeVisible()

    // Restore original status
    await page.getByRole('button', { name: 'Implement auth middleware' }).click()
    await expect(page.locator('h1', { hasText: 'Implement auth middleware' })).toBeVisible()
    await selectStatus(page, 'todo')
  })
})

test.describe('Create Task', () => {
  test('opens and closes create dialog', async ({ page }) => {
    await goToTaskList(page)

    await page.getByText('+ New Task').click()
    await expect(page.getByRole('dialog')).toBeVisible()
    await expect(page.getByPlaceholder('Task title...')).toBeVisible()

    // Close via Cancel (scoped to New Task dialog)
    await page.getByLabel('New Task').getByText('Cancel').click()
    await expect(page.getByPlaceholder('Task title...')).not.toBeVisible()
  })

  test('creates a new task and navigates to detail', async ({ page }) => {
    await goToTaskList(page)

    await page.getByText('+ New Task').click()
    await expect(page.getByRole('dialog')).toBeVisible()

    // Fill form
    await page.getByPlaceholder('Task title...').fill('E2E Test Task')
    await page.getByPlaceholder('Task description (markdown)...').fill('Created by Playwright e2e test')

    // Interactive is the default mode (headless is opt-in checkbox)

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
    await expect(page.getByText('E2E Test Task').first()).toBeVisible()
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
    await expect(page.getByText('Board', { exact: true }).first()).toBeVisible()
  })

  test('clicking tasks nav returns to task list from detail', async ({ page }) => {
    await goToTaskList(page)

    await page.getByRole('button', { name: 'Implement auth middleware' }).click()
    await expect(page.locator('h1', { hasText: 'Implement auth middleware' })).toBeVisible()

    const navTrigger = page.locator('[data-part="trigger"]', { hasText: /Board/ })
    await navTrigger.click()

    await expect(page.locator('h2', { hasText: 'Tasks' })).toBeVisible()
  })
})

test.describe('Task watcher', () => {
  test('board updates when task file is created externally', async ({ page }) => {
    test.fixme(
      process.env.CI === 'true',
      'External fs watcher create events are unreliable on GitHub macOS Wails runs',
    )

    await goToTaskList(page)
    await waitForTasks(page)

    const title = `E2E External Task ${Date.now()}`
    await createExternalTaskFile(title)

    await expect(page.getByRole('heading', { name: title })).toBeVisible({ timeout: 30_000 })
  })
})
