import { test, expect } from '@playwright/test'
import { mkdir, unlink, writeFile } from 'node:fs/promises'
import { join } from 'node:path'
import { homedir } from 'node:os'

// Verifies the fix for the empty-history bug: interactive agent runs persist
// their NDJSON log in raw Anthropic-envelope format. Before the fix,
// TaskDetail unmarshaled those lines into flat StreamEvent and rendered
// labeled-but-empty bubbles. After the fix, interactive runs route through
// ParseConvoLogFile + MessageBubble and surface the original text /
// tool_use / tool_result content.
//
// This test seeds a stopped interactive agent run (both the task file and
// the matching log) then asserts the visible content inside the history
// section — not just bubble count.

const SYBRA_HOME = process.env.SYBRA_HOME ?? join(homedir(), '.sybra')
const TASKS_DIR = join(SYBRA_HOME, 'tasks')
const AGENTS_LOG_DIR = join(SYBRA_HOME, 'logs', 'agents')

const TASK_ID = 'hist0001'
const AGENT_ID = 'hist-agent-1'
const LOG_FILE = join(AGENTS_LOG_DIR, `${AGENT_ID}-2026-04-16T14-35-42.ndjson`)
const TASK_FILE = join(TASKS_DIR, `${TASK_ID}.md`)

const LOG_LINES = [
  { type: 'system', subtype: 'init', session_id: 'sess-hist' },
  {
    type: 'assistant',
    message: {
      content: [
        {
          type: 'text',
          text: 'Starting work on the three-panel workspace task. Let me read the current AgentDetail.',
        },
      ],
    },
  },
  {
    type: 'assistant',
    message: {
      content: [
        {
          type: 'tool_use',
          id: 'tu-hist-1',
          name: 'Bash',
          input: { command: 'ls frontend/src/pages', description: 'List pages' },
        },
      ],
    },
  },
  {
    type: 'user',
    message: {
      content: [
        {
          type: 'tool_result',
          tool_use_id: 'tu-hist-1',
          content: 'AgentDetail.svelte\nTaskDetail.svelte\nProjectDetail.svelte',
        },
      ],
    },
  },
  {
    type: 'result',
    subtype: 'success',
    result: 'Explored the codebase and drafted plan.',
    session_id: 'sess-hist',
    total_cost_usd: 0.1337,
  },
]

const TASK_CONTENT = `---
id: ${TASK_ID}
slug: history-fixture
title: 'history fixture task'
status: human-required
task_type: normal
agent_mode: interactive
allowed_tools: []
tags: [e2e, history]
agent_runs:
  - agent_id: ${AGENT_ID}
    role: implementation
    mode: interactive
    provider: claude
    state: stopped
    started_at: 2026-04-16T14:35:42Z
    cost_usd: 0.1337
    log_file: ${LOG_FILE}
    session_id: sess-hist
created_at: 2026-04-16T14:30:00Z
updated_at: 2026-04-16T14:50:00Z
---
Task with a stopped interactive agent run that has a canned NDJSON log so
Playwright can assert the agent-history UI renders populated bubbles.
`

async function writeFixtures() {
  await mkdir(TASKS_DIR, { recursive: true })
  await mkdir(AGENTS_LOG_DIR, { recursive: true })
  const ndjson = LOG_LINES.map((l) => JSON.stringify(l)).join('\n') + '\n'
  await writeFile(LOG_FILE, ndjson, 'utf8')
  await writeFile(TASK_FILE, TASK_CONTENT, 'utf8')
}

async function cleanupFixtures() {
  await unlink(TASK_FILE).catch(() => {})
  await unlink(LOG_FILE).catch(() => {})
  // Leaving the logs/agents dir in place — other tests may use it.
}

test.describe('TaskDetail agent history', () => {
  test.beforeAll(async () => {
    await writeFixtures()
  })

  test.afterAll(async () => {
    await cleanupFixtures()
  })

  test('interactive agent run renders populated bubbles (not empty labels)', async ({ page }) => {
    await page.goto('/')
    // Navigate into the fixture task via the Board → task card flow.
    await page.locator('[data-part="trigger"]', { hasText: /Board/ }).click()
    await page.locator('button:has(h3)', { hasText: 'history fixture task' }).first().click()

    // Agent history section must be present with the fixture agent.
    const historyHeading = page.getByText('Agent History', { exact: true })
    await expect(historyHeading).toBeVisible({ timeout: 10_000 })

    // Expand the history row.
    const row = page.locator('button', { hasText: AGENT_ID.slice(0, 8) }).first()
    await row.click()

    // The assistant text block must render — this is the regression the fix
    // addresses. Before the fix, bubbles appeared but were empty.
    await expect(
      page.getByText('Starting work on the three-panel workspace task', { exact: false }),
    ).toBeVisible({ timeout: 5_000 })

    // Tool_use block should surface the Bash command.
    await expect(page.getByText('ls frontend/src/pages', { exact: false })).toBeVisible()

    // Tool_result content should appear.
    await expect(page.getByText('AgentDetail.svelte', { exact: false })).toBeVisible()
  })
})

test.describe('ProjectDetail setup tab', () => {
  test('setup commands persist across reload', async ({ page }) => {
    // ProjectDetail uses Wails-backed GetProject + SetProjectSetupCommands.
    // Skip if no projects are registered in the test SYBRA_HOME.
    await page.goto('/')
    const projectsNav = page.locator('[data-part="trigger"]', { hasText: /Projects/ })
    if ((await projectsNav.count()) === 0) {
      test.skip(true, 'Projects nav not present in this SYBRA_HOME')
    }
    await projectsNav.click()
    const firstProject = page.locator('button', { hasText: /github\.com|Automaat|\//i }).first()
    if ((await firstProject.count()) === 0) {
      test.skip(true, 'No projects registered — skipping setup persistence check')
    }
    await firstProject.click()

    const setupTab = page.getByRole('tab', { name: /Setup/i })
    if ((await setupTab.count()) === 0) {
      test.skip(true, 'Setup tab missing — UI build older than this fixture')
    }
    await setupTab.click()

    const textarea = page.locator('textarea').first()
    await expect(textarea).toBeVisible()
    await textarea.fill('# e2e marker\nmise install')
    await page.getByRole('button', { name: 'Save' }).click()
    await expect(page.getByText('Saved', { exact: true })).toBeVisible({ timeout: 5_000 })

    // Reload — setup text must round-trip (comment stripped by save logic).
    await page.reload()
    await setupTab.click()
    await expect(textarea).toHaveValue(/mise install/)
  })
})
