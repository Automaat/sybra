import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, fireEvent, cleanup } from '@testing-library/svelte'
import { PullRequest } from '../../bindings/github.com/Automaat/sybra/internal/github/models.js'

vi.mock('$lib/api', () => ({
  BrowserOpenURL: vi.fn(),
}))

const PRDetailView = (await import('./PRDetailView.svelte')).default

function makePR(overrides: Partial<PullRequest> = {}): PullRequest {
  return PullRequest.createFrom({
    number: 42,
    title: 'Fix auth bug',
    author: 'alice',
    repository: 'org/repo',
    isDraft: false,
    ciStatus: 'SUCCESS',
    reviewDecision: 'APPROVED',
    mergeable: 'MERGEABLE',
    body: '',
    url: 'https://github.com/org/repo/pull/42',
    createdAt: '2026-01-01T10:00:00Z',
    updatedAt: '2026-01-01T11:00:00Z',
    ...overrides,
  })
}

describe('PRDetailView', () => {
  afterEach(() => cleanup())

  it('renders PR title', () => {
    render(PRDetailView, { props: { pr: makePR(), onback: vi.fn() } })
    expect(screen.getByText('Fix auth bug')).toBeDefined()
  })

  it('renders PR number and repo', () => {
    render(PRDetailView, { props: { pr: makePR(), onback: vi.fn() } })
    expect(screen.getByText('org/repo#42')).toBeDefined()
  })

  it('renders author', () => {
    render(PRDetailView, { props: { pr: makePR({ author: 'bob' }), onback: vi.fn() } })
    expect(screen.getByText('by bob')).toBeDefined()
  })

  it('calls onback when Back button clicked', async () => {
    const onback = vi.fn()
    render(PRDetailView, { props: { pr: makePR(), onback } })
    await fireEvent.click(screen.getByText('← Back'))
    expect(onback).toHaveBeenCalled()
  })

  it('shows Draft badge for draft PRs', () => {
    render(PRDetailView, { props: { pr: makePR({ isDraft: true }), onback: vi.fn() } })
    expect(screen.getByText('Draft')).toBeDefined()
  })

  it('does not show Draft badge for non-draft PRs', () => {
    render(PRDetailView, { props: { pr: makePR({ isDraft: false }), onback: vi.fn() } })
    expect(screen.queryByText('Draft')).toBeNull()
  })

  it('shows Approve button when onapprove provided and not yet approved', () => {
    render(PRDetailView, {
      props: { pr: makePR({ reviewDecision: '', viewerHasApproved: false }), onback: vi.fn(), onapprove: vi.fn() },
    })
    expect(screen.getByRole('button', { name: 'Approve' })).toBeDefined()
  })

  it('calls onapprove when Approve clicked', async () => {
    const onapprove = vi.fn()
    render(PRDetailView, {
      props: { pr: makePR({ reviewDecision: '', viewerHasApproved: false }), onback: vi.fn(), onapprove },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Approve' }))
    expect(onapprove).toHaveBeenCalled()
  })

  it('shows Merge button when onmerge provided and PR is eligible', () => {
    render(PRDetailView, { props: { pr: makePR(), onback: vi.fn(), onmerge: vi.fn() } })
    expect(screen.getByRole('button', { name: 'Merge' })).toBeDefined()
  })

  it('does not show Merge when PR is a draft', () => {
    render(PRDetailView, {
      props: { pr: makePR({ isDraft: true }), onback: vi.fn(), onmerge: vi.fn() },
    })
    expect(screen.queryByRole('button', { name: 'Merge' })).toBeNull()
  })

  it('does not show Merge when CI is failing', () => {
    render(PRDetailView, {
      props: { pr: makePR({ ciStatus: 'FAILURE' }), onback: vi.fn(), onmerge: vi.fn() },
    })
    expect(screen.queryByRole('button', { name: 'Merge' })).toBeNull()
  })

  it('shows Rerun Failed button when onrerun provided and CI failed', () => {
    render(PRDetailView, {
      props: { pr: makePR({ ciStatus: 'FAILURE' }), onback: vi.fn(), onrerun: vi.fn() },
    })
    expect(screen.getByRole('button', { name: 'Rerun Failed' })).toBeDefined()
  })

  it('does not show Rerun Failed when CI not failed', () => {
    render(PRDetailView, {
      props: { pr: makePR({ ciStatus: 'SUCCESS' }), onback: vi.fn(), onrerun: vi.fn() },
    })
    expect(screen.queryByRole('button', { name: 'Rerun Failed' })).toBeNull()
  })

  it('shows Fix button when onfix provided and CI failed', () => {
    render(PRDetailView, {
      props: { pr: makePR({ ciStatus: 'FAILURE' }), onback: vi.fn(), onfix: vi.fn() },
    })
    expect(screen.getByRole('button', { name: 'Fix' })).toBeDefined()
  })

  it('shows ci status badge for SUCCESS', () => {
    render(PRDetailView, { props: { pr: makePR({ ciStatus: 'SUCCESS' }), onback: vi.fn() } })
    expect(screen.getByText('CI: success')).toBeDefined()
  })

  it('shows ci status badge for FAILURE', () => {
    render(PRDetailView, { props: { pr: makePR({ ciStatus: 'FAILURE' }), onback: vi.fn() } })
    expect(screen.getByText('CI: failure')).toBeDefined()
  })

  it('shows Conflicts badge when mergeable is CONFLICTING', () => {
    render(PRDetailView, { props: { pr: makePR({ mergeable: 'CONFLICTING' }), onback: vi.fn() } })
    expect(screen.getByText('Conflicts')).toBeDefined()
  })

  it('shows review decision badge as Approved', () => {
    render(PRDetailView, { props: { pr: makePR({ reviewDecision: 'APPROVED' }), onback: vi.fn() } })
    expect(screen.getByText('Approved')).toBeDefined()
  })

  it('renders check runs when provided', () => {
    const checkRuns = [
      { name: 'test-suite', status: 'completed', conclusion: 'success', url: '' },
    ]
    render(PRDetailView, { props: { pr: makePR(), checkRuns, onback: vi.fn() } })
    expect(screen.getByText('test-suite')).toBeDefined()
  })
})
