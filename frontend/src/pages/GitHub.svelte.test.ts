import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'
import type { github } from '../../wailsjs/go/models.js'

const mockLoad = vi.fn()
const mockStartPolling = vi.fn()
const mockStopPolling = vi.fn()

const mockReviewStore = {
  loading: false,
  error: '',
  reviewRequested: [] as github.PullRequest[],
  createdByMe: [] as github.PullRequest[],
  get totalCount() {
    return this.reviewRequested.length + this.createdByMe.length
  },
  load: (...args: unknown[]) => mockLoad(...args),
  startPolling: (...args: unknown[]) => mockStartPolling(...args),
  stopPolling: (...args: unknown[]) => mockStopPolling(...args),
}

const mockRenovateLoad = vi.fn()
const mockRenovateStore = {
  prs: [] as github.RenovatePR[],
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
  issues: [] as github.Issue[],
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
vi.mock('../../wailsjs/go/main/IntegrationService.js', () => ({
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
})
