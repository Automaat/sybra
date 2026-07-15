import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, fireEvent, cleanup } from '@testing-library/svelte'
import { RenovatePR } from '../../bindings/github.com/Automaat/sybra/internal/github/models.js'

vi.mock('../stores/renovate.svelte.js', () => ({
  renovateStore: { load: vi.fn() },
}))

const mockApprove = vi.fn()
const mockMerge = vi.fn()
const mockRerun = vi.fn()
const mockFix = vi.fn()

vi.mock('$lib/api', () => ({
  ApproveRenovatePR: (...args: unknown[]) => mockApprove(...args),
  MergeRenovatePR: (...args: unknown[]) => mockMerge(...args),
  RerunRenovateChecks: (...args: unknown[]) => mockRerun(...args),
  FixRenovateCI: (...args: unknown[]) => mockFix(...args),
}))

const RenovatePRCard = (await import('./RenovatePRCard.svelte')).default

function makePR(overrides: Partial<RenovatePR> = {}): RenovatePR {
  return RenovatePR.createFrom({
    number: 7,
    title: 'Update deps',
    repository: 'org/repo',
    author: 'renovate',
    isDraft: false,
    ciStatus: 'SUCCESS',
    reviewDecision: 'APPROVED',
    mergeable: 'MERGEABLE',
    viewerHasApproved: false,
    labels: [],
    headRefName: 'renovate/deps',
    updatedAt: new Date().toISOString(),
    ...overrides,
  })
}

describe('RenovatePRCard', () => {
  afterEach(() => cleanup())

  it('renders PR title', () => {
    render(RenovatePRCard, { props: { pr: makePR(), onselect: vi.fn() } })
    expect(screen.getByText('Update deps')).toBeDefined()
  })

  it('renders repo and number', () => {
    render(RenovatePRCard, { props: { pr: makePR(), onselect: vi.fn() } })
    expect(screen.getByText('org/repo#7')).toBeDefined()
  })

  it('calls onselect when card clicked', async () => {
    const onselect = vi.fn()
    render(RenovatePRCard, { props: { pr: makePR(), onselect } })
    await fireEvent.click(screen.getByRole('link'))
    expect(onselect).toHaveBeenCalled()
  })

  it('shows Approved badge when reviewDecision is APPROVED', () => {
    render(RenovatePRCard, { props: { pr: makePR({ reviewDecision: 'APPROVED' }), onselect: vi.fn() } })
    expect(screen.getByText('Approved')).toBeDefined()
  })

  it('does not show Approved badge when not approved', () => {
    render(RenovatePRCard, { props: { pr: makePR({ reviewDecision: '' }), onselect: vi.fn() } })
    expect(screen.queryByText('Approved')).toBeNull()
  })

  it('shows Conflicts badge when mergeable is CONFLICTING', () => {
    render(RenovatePRCard, { props: { pr: makePR({ mergeable: 'CONFLICTING' }), onselect: vi.fn() } })
    expect(screen.getByText('Conflicts')).toBeDefined()
  })

  it('renders labels', () => {
    render(RenovatePRCard, {
      props: { pr: makePR({ labels: ['dependencies', 'automerge'] }), onselect: vi.fn() },
    })
    expect(screen.getByText('dependencies')).toBeDefined()
    expect(screen.getByText('automerge')).toBeDefined()
  })

  it('shows Approve button when not yet approved', () => {
    render(RenovatePRCard, {
      props: { pr: makePR({ viewerHasApproved: false, reviewDecision: '' }), onselect: vi.fn() },
    })
    expect(screen.getByRole('button', { name: 'Approve' })).toBeDefined()
  })

  it('hides Approve button when already approved', () => {
    render(RenovatePRCard, {
      props: { pr: makePR({ viewerHasApproved: true }), onselect: vi.fn() },
    })
    expect(screen.queryByRole('button', { name: 'Approve' })).toBeNull()
  })

  it('shows Merge button when eligible', () => {
    render(RenovatePRCard, {
      props: {
        pr: makePR({ isDraft: false, mergeable: 'MERGEABLE', ciStatus: 'SUCCESS', reviewDecision: 'APPROVED' }),
        onselect: vi.fn(),
      },
    })
    expect(screen.getByRole('button', { name: 'Merge' })).toBeDefined()
  })

  it('does not show Merge when draft', () => {
    render(RenovatePRCard, {
      props: { pr: makePR({ isDraft: true }), onselect: vi.fn() },
    })
    expect(screen.queryByRole('button', { name: 'Merge' })).toBeNull()
  })

  it('does not show Merge when CI failing', () => {
    render(RenovatePRCard, {
      props: { pr: makePR({ ciStatus: 'FAILURE' }), onselect: vi.fn() },
    })
    expect(screen.queryByRole('button', { name: 'Merge' })).toBeNull()
  })

  it('does not show Merge while waiting for stability', () => {
    render(RenovatePRCard, {
      props: { pr: makePR({ waitingForStability: true }), onselect: vi.fn() },
    })
    expect(screen.queryByRole('button', { name: 'Merge' })).toBeNull()
    expect(screen.getByText('Stability days').getAttribute('title')).toBe(
      'Waiting for Renovate stability days (minimum release age) before this PR can be merged',
    )
  })

  it('shows Rerun and Fix buttons when CI failed', () => {
    render(RenovatePRCard, {
      props: { pr: makePR({ ciStatus: 'FAILURE' }), onselect: vi.fn() },
    })
    expect(screen.getByRole('button', { name: 'Rerun' })).toBeDefined()
    expect(screen.getByRole('button', { name: 'Fix' })).toBeDefined()
  })

  it('does not show Rerun/Fix when CI success', () => {
    render(RenovatePRCard, {
      props: { pr: makePR({ ciStatus: 'SUCCESS' }), onselect: vi.fn() },
    })
    expect(screen.queryByRole('button', { name: 'Rerun' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Fix' })).toBeNull()
  })

  it('calls ApproveRenovatePR when Approve clicked', async () => {
    mockApprove.mockResolvedValue(undefined)
    render(RenovatePRCard, {
      props: { pr: makePR({ viewerHasApproved: false, reviewDecision: '' }), onselect: vi.fn() },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Approve' }))
    expect(mockApprove).toHaveBeenCalledWith('org/repo', 7)
  })

  it('calls MergeRenovatePR when Merge clicked', async () => {
    mockMerge.mockResolvedValue(undefined)
    render(RenovatePRCard, {
      props: {
        pr: makePR({ isDraft: false, mergeable: 'MERGEABLE', ciStatus: 'SUCCESS', reviewDecision: 'APPROVED' }),
        onselect: vi.fn(),
      },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Merge' }))
    expect(mockMerge).toHaveBeenCalledWith('org/repo', 7)
  })

  it('calls RerunRenovateChecks when Rerun clicked', async () => {
    mockRerun.mockResolvedValue(undefined)
    render(RenovatePRCard, {
      props: { pr: makePR({ ciStatus: 'FAILURE' }), onselect: vi.fn() },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Rerun' }))
    expect(mockRerun).toHaveBeenCalledWith('org/repo', 7)
  })

  it('calls FixRenovateCI when Fix clicked', async () => {
    mockFix.mockResolvedValue(undefined)
    const pr = makePR({ ciStatus: 'FAILURE', headRefName: 'renovate/deps', title: 'Update deps' })
    render(RenovatePRCard, { props: { pr, onselect: vi.fn() } })
    await fireEvent.click(screen.getByRole('button', { name: 'Fix' }))
    expect(mockFix).toHaveBeenCalledWith('org/repo', 7, 'renovate/deps', 'Update deps')
  })
})
