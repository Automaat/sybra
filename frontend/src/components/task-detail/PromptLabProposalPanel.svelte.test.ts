import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const mockApprove = vi.fn()
const mockReject = vi.fn()

vi.mock('../../stores/tasks.svelte.js', () => ({
  taskStore: {
    approveProposal: (...args: unknown[]) => mockApprove(...args),
    rejectProposal: (...args: unknown[]) => mockReject(...args),
  },
}))

const PromptLabProposalPanel = (await import('./PromptLabProposalPanel.svelte')).default

const baseTask = {
  id: 't1',
  title: 'Tighten instructions for role test-runner',
  status: 'human-required',
  body: '**Rationale:** flaky retries.\n\n**Evidence:** 12 runs.',
  tags: ['prompt-lab-proposal', 'role:test-runner', 'requires-human'],
  agentMode: 'headless',
  allowedTools: [],
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
}

describe('PromptLabProposalPanel', () => {
  beforeEach(() => {
    mockApprove.mockReset()
    mockReject.mockReset()
  })
  afterEach(cleanup)

  it('does not render for a non-proposal human-required task', () => {
    const { container } = render(PromptLabProposalPanel, {
      props: { task: { ...baseTask, tags: [] } as never },
    })
    expect(container.querySelector('button')).toBeNull()
  })

  it('does not render once a proposal has left human-required', () => {
    const { container } = render(PromptLabProposalPanel, {
      props: { task: { ...baseTask, status: 'todo' } as never },
    })
    expect(container.querySelector('button')).toBeNull()
  })

  it('renders the proposal body and Approve/Reject buttons', () => {
    render(PromptLabProposalPanel, { props: { task: baseTask as never } })
    expect(screen.getByText(/Rationale/)).toBeDefined()
    expect(screen.getByText(/Evidence/)).toBeDefined()
    expect(screen.getByText('Approve authoring + offline eval')).toBeDefined()
    expect(screen.getByText('Reject')).toBeDefined()
  })

  it('calls approveProposal on Approve click', async () => {
    mockApprove.mockResolvedValue(baseTask)
    render(PromptLabProposalPanel, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByText('Approve authoring + offline eval'))
    expect(mockApprove).toHaveBeenCalledWith('t1')
  })

  it('calls rejectProposal with feedback on Reject click', async () => {
    mockReject.mockResolvedValue(baseTask)
    render(PromptLabProposalPanel, { props: { task: baseTask as never } })
    const textarea = screen.getByPlaceholderText('Rejection feedback (optional)...') as HTMLTextAreaElement
    await fireEvent.input(textarea, { target: { value: 'not worth it' } })
    await fireEvent.click(screen.getByText('Reject'))
    expect(mockReject).toHaveBeenCalledWith('t1', 'not worth it')
  })

  it('calls rejectProposal with empty feedback when none typed', async () => {
    mockReject.mockResolvedValue(baseTask)
    render(PromptLabProposalPanel, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByText('Reject'))
    expect(mockReject).toHaveBeenCalledWith('t1', '')
  })

  it('disables buttons while an action is in flight', async () => {
    let resolveApprove: (value: unknown) => void = () => {}
    mockApprove.mockReturnValue(new Promise((resolve) => { resolveApprove = resolve }))
    render(PromptLabProposalPanel, { props: { task: baseTask as never } })

    const approveButton = screen.getByText('Approve authoring + offline eval') as HTMLButtonElement
    await fireEvent.click(approveButton)
    expect(approveButton.disabled).toBe(true)

    resolveApprove(baseTask)
  })

  it('renders an inline error on approve failure', async () => {
    mockApprove.mockRejectedValue(new Error('task t1 is not a pending prompt-lab proposal (status=todo)'))
    render(PromptLabProposalPanel, { props: { task: baseTask as never } })

    await fireEvent.click(screen.getByText('Approve authoring + offline eval'))
    await vi.waitFor(() => {
      expect(screen.getByText(/not a pending prompt-lab proposal/)).toBeDefined()
    })
  })
})
