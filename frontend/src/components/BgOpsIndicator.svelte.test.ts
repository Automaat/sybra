import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const bgopStoreMock = {
  ops: [] as any[],
  hasActive: false,
  activeCount: 0,
}

vi.mock('../stores/bgops.svelte.js', () => ({
  bgopStore: bgopStoreMock,
}))

const BgOpsIndicator = (await import('./BgOpsIndicator.svelte')).default

function makeOp(overrides: Record<string, unknown> = {}) {
  return {
    id: 'op-1',
    label: 'Cloning repo',
    type: 'clone',
    status: 'running',
    startedAt: new Date().toISOString(),
    completedAt: null,
    phase: '',
    error: '',
    ...overrides,
  }
}

describe('BgOpsIndicator', () => {
  afterEach(() => {
    cleanup()
    bgopStoreMock.ops = []
    bgopStoreMock.hasActive = false
    bgopStoreMock.activeCount = 0
  })

  it('renders nothing when no ops', () => {
    const { container } = render(BgOpsIndicator)
    expect(container.textContent?.trim()).toBe('')
  })

  it('shows button when ops exist', () => {
    bgopStoreMock.ops = [makeOp()]
    render(BgOpsIndicator)
    expect(screen.getByRole('button', { name: 'Background operations' })).toBeDefined()
  })

  it('shows active count when hasActive is true', () => {
    bgopStoreMock.ops = [makeOp()]
    bgopStoreMock.hasActive = true
    bgopStoreMock.activeCount = 2
    render(BgOpsIndicator)
    expect(screen.getByText('2')).toBeDefined()
  })

  it('opens popup on button click', async () => {
    bgopStoreMock.ops = [makeOp({ label: 'Fetching branch' })]
    render(BgOpsIndicator)
    await fireEvent.click(screen.getByRole('button', { name: 'Background operations' }))
    expect(screen.getByText('Fetching branch')).toBeDefined()
  })

  it('shows "Background operations" heading when open', async () => {
    bgopStoreMock.ops = [makeOp()]
    render(BgOpsIndicator)
    await fireEvent.click(screen.getByRole('button', { name: 'Background operations' }))
    expect(screen.getByText('Background operations')).toBeDefined()
  })

  it('shows type label Clone for clone op', async () => {
    bgopStoreMock.ops = [makeOp({ type: 'clone' })]
    render(BgOpsIndicator)
    await fireEvent.click(screen.getByRole('button', { name: 'Background operations' }))
    expect(screen.getByText(/Clone/)).toBeDefined()
  })

  it('shows type label Worktree for worktree op', async () => {
    bgopStoreMock.ops = [makeOp({ type: 'worktree' })]
    render(BgOpsIndicator)
    await fireEvent.click(screen.getByRole('button', { name: 'Background operations' }))
    expect(screen.getByText(/Worktree/)).toBeDefined()
  })

  it('shows error text when op has error', async () => {
    bgopStoreMock.ops = [makeOp({ status: 'failed', error: 'Connection refused' })]
    render(BgOpsIndicator)
    await fireEvent.click(screen.getByRole('button', { name: 'Background operations' }))
    expect(screen.getByText('Connection refused')).toBeDefined()
  })

  it('shows phase when op is running with phase', async () => {
    bgopStoreMock.ops = [makeOp({ status: 'running', phase: 'Resolving refs' })]
    render(BgOpsIndicator)
    await fireEvent.click(screen.getByRole('button', { name: 'Background operations' }))
    expect(screen.getByText('Resolving refs')).toBeDefined()
  })

  it('closes popup on ✕ button click', async () => {
    bgopStoreMock.ops = [makeOp()]
    render(BgOpsIndicator)
    await fireEvent.click(screen.getByRole('button', { name: 'Background operations' }))
    expect(screen.getByText('Background operations')).toBeDefined()
    await fireEvent.click(screen.getByText('✕'))
    expect(screen.queryByText('Background operations')).toBeNull()
  })
})
