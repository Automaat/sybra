/**
 * Screenshot generation for documentation.
 *
 * Run:  npx playwright test e2e/screenshots.spec.ts
 * Output: docs/screenshots/light/*.png  docs/screenshots/dark/*.png
 *
 * Not part of CI — run manually when UI changes warrant updated docs images.
 */
import { test, type Page, type BrowserContext } from '@playwright/test'
import { mockGitHub } from './lib/github-mocks.js'
import { mockProjects } from './lib/project-mocks.js'
import { copyFile, mkdir } from 'node:fs/promises'
import { existsSync } from 'node:fs'
import { join } from 'node:path'
import { homedir } from 'node:os'

const SYBRA_HOME = process.env.SYBRA_HOME ?? join(homedir(), '.sybra')
const TASKS_DIR = join(SYBRA_HOME, 'tasks')
const WORKFLOWS_DIR = join(SYBRA_HOME, 'workflows')

const OUT_DIR = join(import.meta.dirname, '..', '..', 'docs', 'screenshots')

async function shot(page: Page, theme: 'light' | 'dark', name: string) {
  await page.screenshot({
    path: join(OUT_DIR, theme, `${name}.png`),
    fullPage: false,
  })
}

/** Apply color scheme via localStorage (matches the Settings page persistence). */
async function applyTheme(context: BrowserContext, theme: 'light' | 'dark') {
  await context.addInitScript((t) => {
    localStorage.setItem('color-scheme', t)
  }, theme)
}

async function goToTaskList(page: Page) {
  await page.goto('/')
  await page.locator('[data-part="trigger"]', { hasText: /Board/ }).click()
  await page.waitForSelector('button:has(h3), :text("No tasks")', { timeout: 10_000 })
}

test.beforeAll(async () => {
  await mkdir(join(OUT_DIR, 'light'), { recursive: true })
  await mkdir(join(OUT_DIR, 'dark'), { recursive: true })

  // Ensure task fixtures are present
  for (const f of ['auth0001.md', 'test0001.md', 'db0001.md', 'plan0001.md']) {
    const src = join(import.meta.dirname, 'fixtures', f)
    const dst = join(TASKS_DIR, f)
    await copyFile(src, dst)
  }

  // Ensure workflow fixture is present
  if (!existsSync(WORKFLOWS_DIR)) {
    await mkdir(WORKFLOWS_DIR, { recursive: true })
  }
  await copyFile(
    join(import.meta.dirname, 'fixtures', 'wf-editor-e2e.yaml'),
    join(WORKFLOWS_DIR, 'wf-editor-e2e.yaml'),
  )
})

// Run every screenshot block in both themes
for (const theme of ['light', 'dark'] as const) {
  test.describe(theme, () => {
    test.use({ colorScheme: theme, viewport: { width: 1920, height: 1080 } })

    test.beforeEach(async ({ context }) => {
      await applyTheme(context, theme)
    })

    // ─── Dashboard ───────────────────────────────────────────────────────────

    test('dashboard', async ({ page }) => {
      await page.goto('/')
      await page.locator('[data-part="trigger"]', { hasText: /Dashboard/ }).click()
      await page.locator('h1', { hasText: 'Dashboard' }).waitFor()
      await shot(page, theme, 'dashboard')
    })

    // ─── Task Board ───────────────────────────────────────────────────────────

    test('task-board', async ({ page }) => {
      await goToTaskList(page)
      await shot(page, theme, 'task-board')
    })

    test('task-board-new-task-dialog', async ({ page }) => {
      await goToTaskList(page)
      await page.getByText('+ New Task').click()
      await page.getByRole('dialog').waitFor()
      await shot(page, theme, 'task-board-new-task-dialog')
    })

    test('task-board-filtered', async ({ page }) => {
      await goToTaskList(page)
      // Mobile input is hidden at 1920px; .nth(1) targets the visible desktop input
      await page.locator('input[placeholder="Search tasks..."]').nth(1).fill('auth')
      await page.waitForTimeout(300)
      await shot(page, theme, 'task-board-filtered')
    })

    test('quick-add-task', async ({ page }) => {
      await page.goto('/')
      await page.keyboard.press('Meta+n')
      await page.getByPlaceholder('Task title, link, or note...').waitFor()
      await shot(page, theme, 'quick-add-task')
    })

    test('command-palette', async ({ page }) => {
      await page.goto('/')
      await page.keyboard.press('Meta+k')
      // Wait for palette input — CommandPalette opens with a search input
      await page.getByRole('dialog').waitFor({ timeout: 5_000 })
      await shot(page, theme, 'command-palette')
    })

    // ─── Task Detail ──────────────────────────────────────────────────────────

    test('task-detail', async ({ page }) => {
      await goToTaskList(page)
      await page.getByRole('button', { name: 'Implement auth middleware' }).click()
      await page.locator('h1', { hasText: 'Implement auth middleware' }).waitFor()
      await shot(page, theme, 'task-detail')
    })

    test('task-detail-plan-review', async ({ page }) => {
      await goToTaskList(page)
      await page.getByRole('button', { name: 'Refactor logging system' }).click()
      await page.locator('h1', { hasText: 'Refactor logging system' }).waitFor()
      await page.getByRole('button', { name: 'Approve Plan' }).waitFor()
      await shot(page, theme, 'task-detail-plan-review')
    })

    // ─── Reviews ──────────────────────────────────────────────────────────────

    async function goToReviews(page: Page) {
      await page.goto('/')
      await page
        .locator('[data-part="trigger"]', { hasText: /Reviews/ })
        .filter({ hasNotText: 'Test' })
        .click()
      await page.waitForSelector('button, :text("No plans")', { timeout: 10_000 })
    }

    test('reviews', async ({ page }) => {
      await goToReviews(page)
      await shot(page, theme, 'reviews')
    })

    test('reviews-plan-selected', async ({ page }) => {
      await goToReviews(page)
      // Select the plan-review fixture task to show the split panel
      await page.getByText('Refactor logging system').click()
      // Wait for the plan content and action bar to render
      await page.getByRole('button', { name: 'Approve' }).waitFor({ timeout: 5_000 })
      await shot(page, theme, 'reviews-plan-selected')
    })

    // ─── Chats ────────────────────────────────────────────────────────────────

    test('chats', async ({ page }) => {
      await page.goto('/')
      await page.locator('[data-part="trigger"]', { hasText: /Chats/ }).click()
      await page.getByRole('main').getByRole('heading', { name: 'Chats' }).waitFor()
      await shot(page, theme, 'chats')
    })

    test('chats-new-chat-dialog', async ({ page }) => {
      await page.goto('/')
      await page.locator('[data-part="trigger"]', { hasText: /Chats/ }).click()
      await page.getByRole('main').getByRole('heading', { name: 'Chats' }).waitFor()
      await page.getByRole('main').getByRole('button', { name: /New Chat/ }).first().click()
      await page.getByRole('dialog').waitFor()
      await shot(page, theme, 'chats-new-chat-dialog')
    })

    test('chat-detail', async ({ page }) => {
      await page.goto('/')
      await page.locator('[data-part="trigger"]', { hasText: /Chats/ }).click()
      await page.getByRole('main').getByRole('heading', { name: 'Chats' }).waitFor()
      // Open the first existing chat session if present
      const firstChat = page.getByRole('main').getByRole('listitem').first()
      if (await firstChat.count() > 0 && await firstChat.isVisible()) {
        await firstChat.click()
        await page.waitForTimeout(800)
      }
      await shot(page, theme, 'chat-detail')
    })

    // ─── Agents ───────────────────────────────────────────────────────────────

    test('agents', async ({ page }) => {
      await page.goto('/')
      await page.locator('[data-part="trigger"]', { hasText: /Agents/ }).click()
      await page.getByRole('tab', { name: 'Orchestrator' }).waitFor()
      await shot(page, theme, 'agents')
    })

    test('agents-orchestrator', async ({ page }) => {
      await page.goto('/')
      await page.locator('[data-part="trigger"]', { hasText: /Agents/ }).click()
      await page.getByRole('tab', { name: 'Orchestrator' }).waitFor()
      await page.getByRole('tab', { name: 'Orchestrator' }).click()
      // Wait for orchestrator content to settle
      await page.waitForTimeout(500)
      await shot(page, theme, 'agents-orchestrator')
    })

    test('agents-loops', async ({ page }) => {
      await page.goto('/')
      await page.locator('[data-part="trigger"]', { hasText: /Agents/ }).click()
      await page.getByRole('tab', { name: 'Loops' }).waitFor()
      await page.getByRole('tab', { name: 'Loops' }).click()
      await page.waitForTimeout(500)
      await shot(page, theme, 'agents-loops')
    })

    test('agents-loops-new-dialog', async ({ page }) => {
      await page.goto('/')
      await page.locator('[data-part="trigger"]', { hasText: /Agents/ }).click()
      await page.getByRole('tab', { name: 'Loops' }).waitFor()
      await page.getByRole('tab', { name: 'Loops' }).click()
      await page.getByRole('button', { name: '+ New Loop' }).waitFor()
      await page.getByRole('button', { name: '+ New Loop' }).click()
      await page.getByText('New Loop Agent').waitFor()
      await shot(page, theme, 'agents-loops-new-dialog')
    })

    test('agent-detail', async ({ page }) => {
      await page.goto('/')
      await page.locator('[data-part="trigger"]', { hasText: /Agents/ }).click()
      await page.getByRole('tab', { name: 'Orchestrator' }).waitFor()
      // Click the first agent card if any exist — best-effort, skip if none
      const firstCard = page.locator('[data-testid="agent-card"]').first()
      const cardCount = await firstCard.count()
      if (cardCount > 0) {
        await firstCard.click()
        await page.waitForTimeout(800)
        await shot(page, theme, 'agent-detail')
      } else {
        // Fallback: click any card-like button in the agent list
        const anyCard = page.locator('.agent-card, [role="button"]').nth(1)
        if (await anyCard.count() > 0) {
          await anyCard.click()
          await page.waitForTimeout(800)
        }
        await shot(page, theme, 'agent-detail')
      }
    })

    // ─── Projects ─────────────────────────────────────────────────────────────

    test('projects', async ({ page }) => {
      await mockProjects(page)
      await page.goto('/')
      await page.locator('[data-part="trigger"]', { hasText: /Projects/ }).click()
      await page.getByText('automaat/sybra').waitFor({ timeout: 5_000 })
      await shot(page, theme, 'projects')
    })

    async function goToProjectDetail(page: Page) {
      await mockProjects(page)
      await page.goto('/')
      await page.locator('[data-part="trigger"]', { hasText: /Projects/ }).click()
      await page.getByText('automaat/sybra').waitFor()
      await page.getByRole('button', { name: /automaat\/sybra/ }).click()
      // Wait for project detail header (unique to detail view)
      await page.getByText('Clone Path').waitFor({ timeout: 5_000 })
    }

    test('project-detail-tasks', async ({ page }) => {
      await goToProjectDetail(page)
      await shot(page, theme, 'project-detail-tasks')
    })

    test('project-detail-worktrees', async ({ page }) => {
      await goToProjectDetail(page)
      await page.getByText('Worktrees').click()
      await page.waitForTimeout(400)
      await shot(page, theme, 'project-detail-worktrees')
    })

    test('project-detail-sandbox', async ({ page }) => {
      await goToProjectDetail(page)
      await page.getByText('Sandbox').click()
      await page.waitForTimeout(400)
      await shot(page, theme, 'project-detail-sandbox')
    })

    // ─── GitHub ───────────────────────────────────────────────────────────────

    async function goToGitHub(page: Page) {
      await mockGitHub(page)
      await page.goto('/')
      await page.locator('[data-part="trigger"]', { hasText: /GitHub/ }).click()
      // Wait for mocked PR data to appear
      await page.getByText('feat(agent): streaming output improvements').waitFor({ timeout: 10_000 })
    }

    test('github-my-prs', async ({ page }) => {
      await goToGitHub(page)
      await shot(page, theme, 'github-my-prs')
    })

    test('github-reviews', async ({ page }) => {
      await goToGitHub(page)
      await page.getByRole('button', { name: /^Reviews/ }).click()
      await page.getByText('feat(orchestrator): multi-agent task routing').waitFor()
      await shot(page, theme, 'github-reviews')
    })

    test('github-renovate', async ({ page }) => {
      await goToGitHub(page)
      await page.getByRole('button', { name: 'Renovate' }).click()
      await page.getByText('update dependency @skeletonlabs').waitFor()
      await shot(page, theme, 'github-renovate')
    })

    test('github-issues', async ({ page }) => {
      await goToGitHub(page)
      await page.getByRole('button', { name: 'Issues' }).click()
      await page.getByText('Agent output panel should auto-scroll').waitFor()
      await shot(page, theme, 'github-issues')
    })

    test('github-pr-detail', async ({ page }) => {
      await goToGitHub(page)
      // Click the first PR card (feat(agent): streaming output improvements)
      await page.getByText('feat(agent): streaming output improvements').click()
      await page.getByRole('button', { name: '← Back' }).waitFor()
      await shot(page, theme, 'github-pr-detail')
    })

    // ─── Stats ────────────────────────────────────────────────────────────────

    test('stats', async ({ page }) => {
      await page.goto('/')
      await page.locator('[data-part="trigger"]', { hasText: /Stats/ }).click()
      await page.getByRole('main').waitFor()
      await shot(page, theme, 'stats')
    })

    test('stats-this-week', async ({ page }) => {
      await page.goto('/')
      await page.locator('[data-part="trigger"]', { hasText: /Stats/ }).click()
      await page.getByRole('main').waitFor()
      await page.getByRole('button', { name: 'This Week' }).click()
      await page.waitForTimeout(300)
      await shot(page, theme, 'stats-this-week')
    })

    // ─── Workflows ────────────────────────────────────────────────────────────

    test('workflows', async ({ page }) => {
      await page.goto('/')
      await page.locator('[data-part="trigger"]', { hasText: /Workflows/ }).click()
      await page.getByRole('button', { name: /E2E Editor Fixture/ }).waitFor({ timeout: 10_000 })
      await shot(page, theme, 'workflows')
    })

    test('workflow-editor', async ({ page }) => {
      await page.goto('/')
      await page.locator('[data-part="trigger"]', { hasText: /Workflows/ }).click()
      await page.getByRole('button', { name: /E2E Editor Fixture/ }).waitFor({ timeout: 10_000 })
      await page.getByRole('button', { name: /E2E Editor Fixture/ }).click()
      await page.locator('h2', { hasText: 'E2E Editor Fixture' }).waitFor()
      await shot(page, theme, 'workflow-editor')
    })

    test('workflow-editor-trigger-panel', async ({ page }) => {
      await page.goto('/')
      await page.locator('[data-part="trigger"]', { hasText: /Workflows/ }).click()
      await page.getByRole('button', { name: /E2E Editor Fixture/ }).waitFor({ timeout: 10_000 })
      await page.getByRole('button', { name: /E2E Editor Fixture/ }).click()
      await page.locator('h2', { hasText: 'E2E Editor Fixture' }).waitFor()
      await page.locator('.svelte-flow__node-triggerNode').click()
      await shot(page, theme, 'workflow-editor-trigger-panel')
    })

    // ─── Settings ─────────────────────────────────────────────────────────────

    test('settings', async ({ page }) => {
      await page.goto('/')
      await page.locator('[data-part="trigger"]', { hasText: /Settings/ }).click()
      await page.locator('h1', { hasText: 'Settings' }).waitFor()
      await shot(page, theme, 'settings')
    })
  })
}
