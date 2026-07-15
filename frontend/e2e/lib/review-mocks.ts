import type { Page } from '@playwright/test'

const REVIEW_COMMENTS_BY_TASK: Record<string, unknown[]> = {
  plan0001: [
    {
      id: 'c1',
      line: 7,
      body: 'Should we also migrate the middleware package? It has ~40 log.Printf calls that would benefit from structured fields.',
      resolved: false,
      createdAt: '2026-04-16T11:42:00Z',
    },
    {
      id: 'c2',
      line: 11,
      body: 'LOG_LEVEL=trace should be allowed too — useful for local debugging.',
      resolved: false,
      createdAt: '2026-04-16T11:48:00Z',
    },
  ],
}

/**
 * Mocks ListReviewComments so screenshots can show a plan with inline
 * reviewer comments without needing a seeded store on disk.
 */
export async function mockReviewComments(page: Page) {
  await page.route('**/api/ReviewService/ListReviewComments', async (route) => {
    const body = route.request().postData() ?? '[]'
    let taskId = ''
    try {
      const args = JSON.parse(body)
      taskId = Array.isArray(args) ? String(args[0] ?? '') : ''
    } catch {
      // fall through to empty list
    }
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify(REVIEW_COMMENTS_BY_TASK[taskId] ?? []),
    })
  })
}
