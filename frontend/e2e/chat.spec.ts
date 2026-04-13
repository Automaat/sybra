/**
 * Chat UI smoke. Cannot exercise the full stack in CI because:
 *   - no `claude` / `codex` binary is installed,
 *   - no project is registered in the seed fixtures.
 *
 * What this DOES verify is the UX contract that backs the
 * "fix(chat): unblock new chat input + queue" change:
 *   - the New Chat dialog advertises an OPTIONAL prompt and points users
 *     at the "idle chat" path,
 *   - the chat list is reachable from the sidebar.
 *
 * The behavioural assertions (input enabled while running, queue drains
 * on result, etc.) live in the vitest component test
 * `ChatView.svelte.test.ts` and the Go tests in `internal/agent/`.
 */
import { test, expect, type Page } from '@playwright/test'

async function goToChats(page: Page) {
  await page.goto('/')
  await page.locator('[data-part="trigger"]', { hasText: /Chats/ }).click()
  await expect(page.locator('h2', { hasText: 'Chats' })).toBeVisible()
}

test.describe('Chat list', () => {
  test('chat list page renders with new chat entry point', async ({ page }) => {
    await goToChats(page)
    await expect(page.getByRole('button', { name: /New Chat/ })).toBeVisible()
  })

  test('new chat dialog exposes optional prompt for idle-start chats', async ({ page }) => {
    await goToChats(page)
    await page.getByRole('button', { name: /New Chat/ }).first().click()

    await expect(page.getByText('New Chat', { exact: true })).toBeVisible()

    // The optional-prompt copy is the user-facing contract: it tells the
    // user they can skip the prompt and land on an idle chat. The fix
    // depends on that flow producing a typeable input.
    await expect(page.getByPlaceholder('Leave empty to land in an idle chat...')).toBeVisible()
    await expect(page.getByText(/Prompt/).first()).toBeVisible()
    await expect(page.getByText(/optional/i).first()).toBeVisible()

    // Provider toggle must offer both options so chat works for the
    // active provider matrix.
    await expect(page.getByLabel('Claude')).toBeVisible()
    await expect(page.getByLabel('Codex')).toBeVisible()
  })
})
