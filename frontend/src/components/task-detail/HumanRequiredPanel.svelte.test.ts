import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const mockDispatch = vi.fn()

vi.mock('../../stores/tasks.svelte.js', () => ({
  taskStore: {
    dispatchFromHumanRequired: (...args: unknown[]) => mockDispatch(...args),
  },
}))

const HumanRequiredPanel = (await import('./HumanRequiredPanel.svelte')).default

const baseTask = {
  id: 't1',
  title: 'Fix it',
  status: 'human-required',
  body: '',
  statusReason: '',
  prNumber: 0,
  tags: [],
  agentMode: 'headless',
  allowedTools: [],
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
}

async function typeReason(value: string) {
  const textarea = screen.getByPlaceholderText('Decision reason (required)...') as HTMLTextAreaElement
  await fireEvent.input(textarea, { target: { value } })
}

describe('HumanRequiredPanel', () => {
  beforeEach(() => {
    mockDispatch.mockReset()
  })
  afterEach(cleanup)

  it('does not render when status is not human-required', () => {
    const { container } = render(HumanRequiredPanel, {
      props: { task: { ...baseTask, status: 'todo' } as never },
    })
    expect(container.querySelector('button')).toBeNull()
  })

  it('disables dispatch buttons until a reason is typed', async () => {
    render(HumanRequiredPanel, { props: { task: baseTask as never } })
    const button = screen.getByText('Re-run implementation') as HTMLButtonElement
    expect(button.disabled).toBe(true)

    await typeReason('looks fine')
    expect(button.disabled).toBe(false)
  })

  it('hides the link-PR button when prNumber is 0', () => {
    render(HumanRequiredPanel, { props: { task: baseTask as never } })
    expect(screen.queryByText('Link PR and review')).toBeNull()
  })

  it('shows the link-PR button when prNumber is greater than 0', () => {
    render(HumanRequiredPanel, { props: { task: { ...baseTask, prNumber: 42 } as never } })
    expect(screen.getByText('Link PR and review')).toBeDefined()
  })

  it('dispatches the correct target per button', async () => {
    mockDispatch.mockResolvedValue(baseTask)
    render(HumanRequiredPanel, { props: { task: { ...baseTask, prNumber: 42 } as never } })
    await typeReason('retry please')

    await fireEvent.click(screen.getByText('Send to testing'))
    expect(mockDispatch).toHaveBeenCalledWith('t1', 'testing', 'retry please')
  })

  it('sends the in-review target from the link-PR button', async () => {
    mockDispatch.mockResolvedValue(baseTask)
    render(HumanRequiredPanel, { props: { task: { ...baseTask, prNumber: 42 } as never } })
    await typeReason('pr exists')

    await fireEvent.click(screen.getByText('Link PR and review'))
    expect(mockDispatch).toHaveBeenCalledWith('t1', 'in-review', 'pr exists')
  })

  it('disables buttons while a dispatch is in flight', async () => {
    let resolveDispatch: (value: unknown) => void = () => {}
    mockDispatch.mockReturnValue(new Promise((resolve) => { resolveDispatch = resolve }))
    render(HumanRequiredPanel, { props: { task: baseTask as never } })
    await typeReason('retry please')

    const button = screen.getByText('Re-run implementation') as HTMLButtonElement
    await fireEvent.click(button)
    expect(button.disabled).toBe(true)

    resolveDispatch(baseTask)
  })

  it('renders an inline error on dispatch failure', async () => {
    mockDispatch.mockRejectedValue(new Error('dispatch failed: no workflow matched'))
    render(HumanRequiredPanel, { props: { task: baseTask as never } })
    await typeReason('retry please')

    await fireEvent.click(screen.getByText('Re-run implementation'))
    await vi.waitFor(() => {
      expect(screen.getByText(/dispatch failed: no workflow matched/)).toBeDefined()
    })
  })

  it('renders the operator status_reason', () => {
    render(HumanRequiredPanel, {
      props: { task: { ...baseTask, statusReason: 'tests failed twice' } as never },
    })
    expect(screen.getByText('tests failed twice')).toBeDefined()
  })

  it('shows a Bless hint on tamper-flagged tasks alongside the generic dispatch actions', () => {
    render(HumanRequiredPanel, {
      props: { task: { ...baseTask, tamperFlagged: true, prNumber: 42 } as never },
    })
    expect(screen.getByText(/tamper-flagged/i)).toBeDefined()
    expect(screen.getByPlaceholderText('Decision reason (required)...')).toBeDefined()
    expect(screen.getByText('Re-run implementation')).toBeDefined()
    expect(screen.getByText('Send to testing')).toBeDefined()
    expect(screen.getByText('Open PR')).toBeDefined()
    expect(screen.getByText('Link PR and review')).toBeDefined()
  })

  it('renders the auto-review verdict section from the body when present', () => {
    const body = [
      'Some description.',
      '',
      '## Auto-review verdict: needs human',
      '',
      'The implementation looks incomplete.',
      '',
      '## Unrelated Section',
      'noise',
    ].join('\n')
    render(HumanRequiredPanel, { props: { task: { ...baseTask, body } as never } })
    expect(screen.getByText(/The implementation looks incomplete\./)).toBeDefined()
    expect(screen.queryByText(/noise/)).toBeNull()
  })
})
