import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const mockBrowserOpenURL = vi.fn()

vi.mock('$lib/api', () => ({
  BrowserOpenURL: (...args: unknown[]) => mockBrowserOpenURL(...args),
}))

const IssueCard = (await import('./IssueCard.svelte')).default

function makeIssue(overrides: Record<string, unknown> = {}) {
  return {
    id: 1,
    number: 42,
    title: 'Fix login bug',
    url: 'https://github.com/org/repo/issues/42',
    repository: 'org/repo',
    author: 'alice',
    labels: [],
    updatedAt: '2026-04-01T00:00:00Z',
    body: '',
    state: 'open',
    ...overrides,
  }
}

describe('IssueCard', () => {
  beforeEach(() => {
    mockBrowserOpenURL.mockClear()
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-04-01T12:00:00Z'))
  })

  afterEach(() => {
    cleanup()
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('renders issue title', () => {
    render(IssueCard, { props: { issue: makeIssue() } })
    expect(screen.getByText('Fix login bug')).toBeDefined()
  })

  it('renders repository and issue number', () => {
    render(IssueCard, { props: { issue: makeIssue() } })
    expect(screen.getByText('org/repo#42')).toBeDefined()
  })

  it('renders author', () => {
    render(IssueCard, { props: { issue: makeIssue() } })
    expect(screen.getByText('by alice')).toBeDefined()
  })

  it('renders labels when present', () => {
    render(IssueCard, { props: { issue: makeIssue({ labels: ['bug', 'backend'] }) } })
    expect(screen.getByText('bug')).toBeDefined()
    expect(screen.getByText('backend')).toBeDefined()
  })

  it('renders no label spans when labels array is empty', () => {
    render(IssueCard, { props: { issue: makeIssue({ labels: [] }) } })
    expect(screen.queryByText('bug')).toBeNull()
  })

  it('calls BrowserOpenURL with issue URL on click', async () => {
    const issue = makeIssue({ url: 'https://github.com/org/repo/issues/42' })
    render(IssueCard, { props: { issue } })
    const card = document.querySelector('[role="link"]')
    await fireEvent.click(card!)
    expect(mockBrowserOpenURL).toHaveBeenCalledWith('https://github.com/org/repo/issues/42')
  })

  it('calls BrowserOpenURL on Enter keydown', async () => {
    const issue = makeIssue({ url: 'https://github.com/org/repo/issues/42' })
    render(IssueCard, { props: { issue } })
    const card = document.querySelector('[role="link"]')
    await fireEvent.keyDown(card!, { key: 'Enter' })
    expect(mockBrowserOpenURL).toHaveBeenCalledWith('https://github.com/org/repo/issues/42')
  })

  it('does not call BrowserOpenURL on other key press', async () => {
    render(IssueCard, { props: { issue: makeIssue() } })
    const card = document.querySelector('[role="link"]')
    await fireEvent.keyDown(card!, { key: 'Space' })
    expect(mockBrowserOpenURL).not.toHaveBeenCalled()
  })

  describe('timeAgo', () => {
    it('shows "just now" for very recent updates', () => {
      vi.setSystemTime(new Date('2026-04-01T00:00:30Z'))
      render(IssueCard, { props: { issue: makeIssue({ updatedAt: '2026-04-01T00:00:00Z' }) } })
      expect(screen.getByText('just now')).toBeDefined()
    })

    it('shows "Xm ago" for updates within an hour', () => {
      vi.setSystemTime(new Date('2026-04-01T00:10:00Z'))
      render(IssueCard, { props: { issue: makeIssue({ updatedAt: '2026-04-01T00:00:00Z' }) } })
      expect(screen.getByText('10m ago')).toBeDefined()
    })

    it('shows "Xh ago" for updates within a day', () => {
      vi.setSystemTime(new Date('2026-04-01T05:00:00Z'))
      render(IssueCard, { props: { issue: makeIssue({ updatedAt: '2026-04-01T00:00:00Z' }) } })
      expect(screen.getByText('5h ago')).toBeDefined()
    })

    it('shows "Xd ago" for older updates', () => {
      vi.setSystemTime(new Date('2026-04-04T00:00:00Z'))
      render(IssueCard, { props: { issue: makeIssue({ updatedAt: '2026-04-01T00:00:00Z' }) } })
      expect(screen.getByText('3d ago')).toBeDefined()
    })

    it('shows empty string for missing date', () => {
      render(IssueCard, { props: { issue: makeIssue({ updatedAt: '' }) } })
      expect(screen.queryByText(/ago/)).toBeNull()
    })
  })
})
