import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'

const mockLoad = vi.fn()
const mockStartPolling = vi.fn()
const mockStopPolling = vi.fn()

const mockReviewStore = {
  loading: false,
  error: '',
  reviewRequested: [] as any[],
  createdByMe: [] as any[],
  get totalCount() {
    return this.reviewRequested.length + this.createdByMe.length
  },
  load: (...args: unknown[]) => mockLoad(...args),
  startPolling: (...args: unknown[]) => mockStartPolling(...args),
  stopPolling: (...args: unknown[]) => mockStopPolling(...args),
}

vi.mock('../stores/reviews.svelte.js', () => ({
  reviewStore: mockReviewStore,
}))

vi.mock('../components/PRCard.svelte', () => ({ default: () => {} }))

const GitHub = (await import('./GitHub.svelte')).default

describe('GitHub', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mockReviewStore.loading = false
    mockReviewStore.error = ''
    mockReviewStore.reviewRequested = []
    mockReviewStore.createdByMe = []
  })

  afterEach(() => {
    cleanup()
  })

  it('renders Review Requested section', () => {
    render(GitHub, { props: {} })
    expect(screen.getByText('Review Requested')).toBeDefined()
  })

  it('renders My PRs section', () => {
    render(GitHub, { props: {} })
    expect(screen.getByText('My PRs')).toBeDefined()
  })

  it('shows PR count', () => {
    render(GitHub, { props: {} })
    expect(screen.getByText('0 pull requests')).toBeDefined()
  })

  it('shows singular pull request text', () => {
    mockReviewStore.createdByMe = [{ url: 'https://github.com/o/r/pull/1', number: 1, repository: 'o/r' }]
    render(GitHub, { props: {} })
    expect(screen.getByText('1 pull request')).toBeDefined()
  })

  it('shows empty review requested message', () => {
    render(GitHub, { props: {} })
    expect(screen.getByText('No pending review requests')).toBeDefined()
  })

  it('shows empty my PRs message', () => {
    render(GitHub, { props: {} })
    expect(screen.getByText('No open pull requests')).toBeDefined()
  })

  it('shows loading when loading with no items', () => {
    mockReviewStore.loading = true
    render(GitHub, { props: {} })
    expect(screen.getByText('Loading pull requests...')).toBeDefined()
  })

  it('shows error message', () => {
    mockReviewStore.error = 'API error'
    render(GitHub, { props: {} })
    expect(screen.getByText('API error')).toBeDefined()
  })

  it('shows Refresh button', () => {
    render(GitHub, { props: {} })
    expect(screen.getByText('Refresh')).toBeDefined()
  })

  it('calls load on mount', () => {
    render(GitHub, { props: {} })
    expect(mockLoad).toHaveBeenCalled()
  })
})
