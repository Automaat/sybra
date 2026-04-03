import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, cleanup } from '@testing-library/svelte'
import PRCard from './PRCard.svelte'

const mockBrowserOpenURL = vi.fn()

vi.mock('../../wailsjs/runtime/runtime.js', () => ({
  BrowserOpenURL: (...args: unknown[]) => mockBrowserOpenURL(...args),
}))

function makePR(overrides: Record<string, unknown> = {}) {
  return {
    number: 42,
    title: 'Add feature',
    url: 'https://github.com/org/repo/pull/42',
    repository: 'org/repo',
    repoName: 'repo',
    author: 'dev',
    isDraft: false,
    labels: [],
    ciStatus: '',
    reviewDecision: '',
    unresolvedCount: 0,
    createdAt: '2026-04-01T00:00:00Z',
    updatedAt: '2026-04-01T00:00:00Z',
    ...overrides,
  }
}

describe('PRCard', () => {
  beforeEach(() => {
    vi.clearAllMocks()
  })

  afterEach(() => {
    cleanup()
  })

  it('renders PR title', () => {
    render(PRCard, { props: { pr: makePR() } })
    expect(screen.getByText('Add feature')).toBeDefined()
  })

  it('renders repo and number', () => {
    render(PRCard, { props: { pr: makePR() } })
    expect(screen.getByText('org/repo#42')).toBeDefined()
  })

  it('renders author', () => {
    render(PRCard, { props: { pr: makePR() } })
    expect(screen.getByText('by dev')).toBeDefined()
  })

  it('opens URL on click', async () => {
    render(PRCard, { props: { pr: makePR() } })
    await fireEvent.click(screen.getByRole('button'))
    expect(mockBrowserOpenURL).toHaveBeenCalledWith('https://github.com/org/repo/pull/42')
  })

  it('shows Draft badge when draft', () => {
    render(PRCard, { props: { pr: makePR({ isDraft: true }) } })
    expect(screen.getByText('Draft')).toBeDefined()
  })

  it('hides Draft badge when not draft', () => {
    render(PRCard, { props: { pr: makePR({ isDraft: false }) } })
    expect(screen.queryByText('Draft')).toBeNull()
  })

  it('renders labels', () => {
    render(PRCard, { props: { pr: makePR({ labels: ['bug', 'urgent'] }) } })
    expect(screen.getByText('bug')).toBeDefined()
    expect(screen.getByText('urgent')).toBeDefined()
  })

  describe('CI status', () => {
    it('shows green dot for SUCCESS', () => {
      render(PRCard, { props: { pr: makePR({ ciStatus: 'SUCCESS' }) } })
      const dot = document.querySelector('[title="CI: success"]')
      expect(dot).toBeDefined()
      expect(dot?.className).toContain('bg-green-500')
    })

    it('shows red dot for FAILURE', () => {
      render(PRCard, { props: { pr: makePR({ ciStatus: 'FAILURE' }) } })
      const dot = document.querySelector('[title="CI: failure"]')
      expect(dot).toBeDefined()
      expect(dot?.className).toContain('bg-red-500')
    })

    it('shows yellow dot for PENDING', () => {
      render(PRCard, { props: { pr: makePR({ ciStatus: 'PENDING' }) } })
      const dot = document.querySelector('[title="CI: pending"]')
      expect(dot).toBeDefined()
      expect(dot?.className).toContain('bg-yellow-500')
    })

    it('shows no dot when empty', () => {
      render(PRCard, { props: { pr: makePR({ ciStatus: '' }) } })
      expect(document.querySelector('[title^="CI:"]')).toBeNull()
    })
  })

  describe('review status', () => {
    it('shows Approved badge', () => {
      render(PRCard, { props: { pr: makePR({ reviewDecision: 'APPROVED' }) } })
      expect(screen.getByText('Approved')).toBeDefined()
    })

    it('shows Changes badge', () => {
      render(PRCard, { props: { pr: makePR({ reviewDecision: 'CHANGES_REQUESTED' }) } })
      expect(screen.getByText('Changes')).toBeDefined()
    })

    it('shows no review badge for REVIEW_REQUIRED', () => {
      render(PRCard, { props: { pr: makePR({ reviewDecision: 'REVIEW_REQUIRED' }) } })
      expect(screen.queryByText('Approved')).toBeNull()
      expect(screen.queryByText('Changes')).toBeNull()
    })

    it('shows unresolved count', () => {
      render(PRCard, { props: { pr: makePR({ unresolvedCount: 3 }) } })
      expect(screen.getByText('3 unresolved')).toBeDefined()
    })

    it('hides unresolved when zero', () => {
      render(PRCard, { props: { pr: makePR({ unresolvedCount: 0 }) } })
      expect(screen.queryByText(/unresolved/)).toBeNull()
    })
  })

  describe('timeAgo', () => {
    beforeEach(() => {
      vi.useFakeTimers()
    })

    afterEach(() => {
      vi.useRealTimers()
      cleanup()
    })

    it('shows "just now" for <60s', () => {
      vi.setSystemTime(new Date('2026-04-01T00:00:30Z'))
      render(PRCard, { props: { pr: makePR() } })
      expect(screen.getByText('just now')).toBeDefined()
    })

    it('shows minutes for <1h', () => {
      vi.setSystemTime(new Date('2026-04-01T00:10:00Z'))
      render(PRCard, { props: { pr: makePR() } })
      expect(screen.getByText('10m ago')).toBeDefined()
    })

    it('shows hours for <24h', () => {
      vi.setSystemTime(new Date('2026-04-01T05:00:00Z'))
      render(PRCard, { props: { pr: makePR() } })
      expect(screen.getByText('5h ago')).toBeDefined()
    })

    it('shows days for >=24h', () => {
      vi.setSystemTime(new Date('2026-04-04T00:00:00Z'))
      render(PRCard, { props: { pr: makePR() } })
      expect(screen.getByText('3d ago')).toBeDefined()
    })
  })
})
