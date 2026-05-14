import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'
import type { Issue, PullRequest, RenovatePR } from '../../bindings/github.com/Automaat/sybra/internal/github/models.js'

const mockLoad = vi.fn()
const mockStartPolling = vi.fn()
const mockStopPolling = vi.fn()

const mockReviewStore = {
  loading: false,
  error: '',
  reviewRequested: [] as PullRequest[],
  createdByMe: [] as PullRequest[],
  get totalCount() {
    return this.reviewRequested.length + this.createdByMe.length
  },
  load: (...args: unknown[]) => mockLoad(...args),
  startPolling: (...args: unknown[]) => mockStartPolling(...args),
  stopPolling: (...args: unknown[]) => mockStopPolling(...args),
}

const mockRenovateLoad = vi.fn()
const mockRenovateStore = {
  prs: [] as RenovatePR[],
  loading: false,
  error: '',
  get count() {
    return this.prs.length
  },
  get eligible() {
    return []
  },
  get failing() {
    return []
  },
  load: (...args: unknown[]) => mockRenovateLoad(...args),
  listen: vi.fn(),
  stopListening: vi.fn(),
  startPolling: vi.fn(),
  stopPolling: vi.fn(),
}

vi.mock('../stores/reviews.svelte.js', () => ({
  reviewStore: mockReviewStore,
}))

vi.mock('../stores/renovate.svelte.js', () => ({
  renovateStore: mockRenovateStore,
}))

const mockIssueStore = {
  issues: [] as Issue[],
  loading: false,
  error: '',
  get count() {
    return this.issues.length
  },
  load: vi.fn(),
  listen: vi.fn(),
  stopListening: vi.fn(),
  startPolling: vi.fn(),
  stopPolling: vi.fn(),
}

vi.mock('../stores/issues.svelte.js', () => ({
  issueStore: mockIssueStore,
}))

vi.mock('../components/PRCard.svelte', () => ({ default: () => {} }))
vi.mock('../components/RenovatePRCard.svelte', () => ({ default: () => {} }))
vi.mock('../components/IssueCard.svelte', () => ({ default: () => {} }))
vi.mock('../components/PRDetailView.svelte', () => ({ default: () => {} }))
vi.mock('$lib/api', () => ({
  ApproveRenovatePR: vi.fn(),
  MergeRenovatePR: vi.fn(),
  RerunRenovateChecks: vi.fn(),
  FixRenovateCI: vi.fn(),
}))

const GitHub = (await import('./GitHub.svelte')).default

describe('GitHub', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mockReviewStore.loading = false
    mockReviewStore.error = ''
    mockReviewStore.reviewRequested = []
    mockReviewStore.createdByMe = []
    mockRenovateStore.prs = []
    mockRenovateStore.loading = false
    mockRenovateStore.error = ''
    mockIssueStore.issues = []
    mockIssueStore.loading = false
    mockIssueStore.error = ''
  })

  afterEach(() => {
    cleanup()
  })

  it('renders tab bar with My PRs, Reviews, Renovate, Issues', () => {
    render(GitHub, { props: {} })
    expect(screen.getByText('My PRs')).toBeDefined()
    expect(screen.getByText('Reviews')).toBeDefined()
    expect(screen.getByText('Renovate')).toBeDefined()
    expect(screen.getByText('Issues')).toBeDefined()
  })

  it('shows empty my PRs message on default tab', () => {
    render(GitHub, { props: {} })
    expect(screen.getByText('No open pull requests')).toBeDefined()
  })

  it('shows Refresh button', () => {
    render(GitHub, { props: {} })
    expect(screen.getByText('Refresh')).toBeDefined()
  })

  it('calls load on mount', () => {
    render(GitHub, { props: {} })
    expect(mockLoad).toHaveBeenCalled()
    expect(mockRenovateLoad).toHaveBeenCalled()
  })

  it('switches to Reviews tab', async () => {
    render(GitHub, { props: {} })
    await fireEvent.click(screen.getByText('Reviews'))
    expect(screen.getByText('No pending review requests')).toBeDefined()
  })

  it('switches to Renovate tab', async () => {
    render(GitHub, { props: {} })
    await fireEvent.click(screen.getByText('Renovate'))
    expect(screen.getByText('No Renovate PRs')).toBeDefined()
  })

  it('switches to Issues tab', async () => {
    render(GitHub, { props: {} })
    await fireEvent.click(screen.getByText('Issues'))
    expect(screen.getByText('No assigned issues')).toBeDefined()
  })

  it('shows Loading state on My PRs while loading', () => {
    mockReviewStore.loading = true
    render(GitHub, { props: {} })
    expect(screen.getByText('Loading...')).toBeDefined()
  })

  it('shows Loading state on Renovate tab while loading', async () => {
    mockRenovateStore.loading = true
    render(GitHub, { props: {} })
    await fireEvent.click(screen.getByText('Renovate'))
    expect(screen.getByText('Loading...')).toBeDefined()
  })

  it('shows error and Retry button when Renovate fails', async () => {
    mockRenovateStore.error = 'rate-limited'
    render(GitHub, { props: {} })
    await fireEvent.click(screen.getByText('Renovate'))
    expect(screen.getByText('Failed to load Renovate PRs')).toBeDefined()
    expect(screen.getByText('rate-limited')).toBeDefined()
    expect(screen.getByText('Retry')).toBeDefined()
  })

  it('clicking Retry on Renovate calls renovateStore.load', async () => {
    mockRenovateStore.error = 'rate-limited'
    render(GitHub, { props: {} })
    await fireEvent.click(screen.getByText('Renovate'))
    mockRenovateLoad.mockClear()
    await fireEvent.click(screen.getByText('Retry'))
    expect(mockRenovateLoad).toHaveBeenCalled()
  })

  it('shows issue store error on Issues tab', async () => {
    mockIssueStore.error = 'github 401'
    render(GitHub, { props: {} })
    await fireEvent.click(screen.getByText('Issues'))
    expect(screen.getByText('github 401')).toBeDefined()
  })

  it('clicking Refresh on My PRs calls reviewStore.load', async () => {
    render(GitHub, { props: {} })
    mockLoad.mockClear()
    await fireEvent.click(screen.getByText('Refresh'))
    expect(mockLoad).toHaveBeenCalled()
  })

  it('clicking Refresh on Renovate tab calls renovateStore.load', async () => {
    render(GitHub, { props: {} })
    await fireEvent.click(screen.getByText('Renovate'))
    mockRenovateLoad.mockClear()
    await fireEvent.click(screen.getByText('Refresh'))
    expect(mockRenovateLoad).toHaveBeenCalled()
  })

  it('clicking Refresh on Issues tab calls issueStore.load', async () => {
    render(GitHub, { props: {} })
    await fireEvent.click(screen.getByText('Issues'))
    vi.mocked(mockIssueStore.load).mockClear()
    await fireEvent.click(screen.getByText('Refresh'))
    expect(mockIssueStore.load).toHaveBeenCalled()
  })

  it('shows tab badge count when PRs are present', () => {
    mockReviewStore.createdByMe = [
      { url: 'u1', repository: 'owner/repo', number: 1, title: 'PR1' } as unknown as PullRequest,
      { url: 'u2', repository: 'owner/repo', number: 2, title: 'PR2' } as unknown as PullRequest,
    ]
    render(GitHub, { props: {} })
    expect(screen.getByText('2')).toBeDefined()
  })

  it('groups My PRs by repository', () => {
    mockReviewStore.createdByMe = [
      { url: 'u1', repository: 'org/a', number: 1, title: 'A1', isDraft: false, mergeable: 'MERGEABLE', ciStatus: 'SUCCESS', reviewDecision: 'APPROVED' } as unknown as PullRequest,
      { url: 'u2', repository: 'org/b', number: 2, title: 'B1', isDraft: false, mergeable: 'MERGEABLE', ciStatus: 'SUCCESS', reviewDecision: 'APPROVED' } as unknown as PullRequest,
    ]
    render(GitHub, { props: {} })
    expect(screen.getByText('org/a')).toBeDefined()
    expect(screen.getByText('org/b')).toBeDefined()
  })

  it('shows Renovate search input on Renovate tab', async () => {
    render(GitHub, { props: {} })
    await fireEvent.click(screen.getByText('Renovate'))
    expect(screen.getByPlaceholderText(/Search by repo/)).toBeDefined()
  })

  it('filters Renovate PRs by search query', async () => {
    mockRenovateStore.prs = [
      { url: 'u1', repository: 'org/match', number: 1, title: 'Update foo', labels: ['deps'] } as unknown as RenovatePR,
      { url: 'u2', repository: 'org/other', number: 2, title: 'Update bar', labels: ['deps'] } as unknown as RenovatePR,
    ]
    render(GitHub, { props: {} })
    await fireEvent.click(screen.getByText('Renovate'))
    const input = screen.getByPlaceholderText(/Search by repo/)
    await fireEvent.input(input, { target: { value: 'match' } })
    await vi.waitFor(() => {
      expect(screen.getByText('org/match')).toBeDefined()
      expect(screen.queryByText('org/other')).toBeNull()
    })
  })

  it('shows no-match message when Renovate search has no results', async () => {
    mockRenovateStore.prs = [
      { url: 'u1', repository: 'org/foo', number: 1, title: 'Update foo', labels: [] } as unknown as RenovatePR,
    ]
    render(GitHub, { props: {} })
    await fireEvent.click(screen.getByText('Renovate'))
    const input = screen.getByPlaceholderText(/Search by repo/)
    await fireEvent.input(input, { target: { value: 'nomatchatall' } })
    await vi.waitFor(() => {
      expect(screen.getByText(/No matches for "nomatchatall"/)).toBeDefined()
    })
  })

  it('Escape in Renovate search clears query', async () => {
    render(GitHub, { props: {} })
    await fireEvent.click(screen.getByText('Renovate'))
    const input = screen.getByPlaceholderText(/Search by repo/) as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'query' } })
    expect(input.value).toBe('query')
    await fireEvent.keyDown(input, { key: 'Escape' })
    await vi.waitFor(() => {
      expect(input.value).toBe('')
    })
  })

  it('stops polling on unmount', () => {
    const { unmount } = render(GitHub, { props: {} })
    unmount()
    expect(mockStopPolling).toHaveBeenCalled()
  })

  it('responds to focus-renovate-search window event by switching to Renovate tab', async () => {
    render(GitHub, { props: {} })
    window.dispatchEvent(new CustomEvent('focus-renovate-search'))
    await vi.waitFor(() => {
      expect(screen.getByPlaceholderText(/Search by repo/)).toBeDefined()
    })
  })
})
