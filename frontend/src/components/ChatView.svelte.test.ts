import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'
import { SvelteMap } from 'svelte/reactivity'

const mockGetOutput = vi.fn()
const mockSendMessage = vi.fn()
const mockRespondApproval = vi.fn()
const mockSubscribe = vi.fn((..._args: unknown[]) => () => {})

const conversations = new SvelteMap<string, unknown[]>()
const pendingApprovals = new SvelteMap<string, SvelteMap<string, unknown>>()

vi.mock('../stores/convo.svelte.js', () => ({
  convoStore: {
    conversations,
    pendingApprovals,
    approvalsFor: (agentId: string) => [...(pendingApprovals.get(agentId)?.values() ?? [])],
    getOutput: (...args: unknown[]) => mockGetOutput(...args),
    sendMessage: (...args: unknown[]) => mockSendMessage(...args),
    respondApproval: (...args: unknown[]) => mockRespondApproval(...args),
    subscribe: (...args: unknown[]) => mockSubscribe(...args),
  },
}))

const ChatView = (await import('./ChatView.svelte')).default

describe('ChatView', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    conversations.clear()
    pendingApprovals.clear()
    mockGetOutput.mockResolvedValue([])
  })

  afterEach(() => {
    cleanup()
  })

  // Bug regression: a fresh chat starts with state=running and zero events.
  // The textarea must NOT be disabled — otherwise the user is locked out and
  // cannot send the first message (the "Waiting for response..." deadlock).
  it('keeps input enabled when running with no events', async () => {
    render(ChatView, { props: { agentId: 'a1', agentState: 'running' } })

    const textarea = await screen.findByRole('textbox')
    expect((textarea as HTMLTextAreaElement).disabled).toBe(false)
    expect((textarea as HTMLTextAreaElement).placeholder).toBe('Queue a follow-up...')
  })

  it('keeps input enabled when paused without approval', async () => {
    render(ChatView, { props: { agentId: 'a1', agentState: 'paused' } })

    const textarea = await screen.findByRole('textbox')
    expect((textarea as HTMLTextAreaElement).disabled).toBe(false)
    expect((textarea as HTMLTextAreaElement).placeholder).toBe('Type a message...')
  })

  // Approval is the only state where the input must be hard-locked: typing
  // a follow-up while a tool-use is awaiting consent would race the queue
  // ahead of the approval handshake.
  it('disables input when a tool approval is pending', async () => {
    pendingApprovals.set('a1', new SvelteMap([['tool-1', {
      toolUseId: 'tool-1',
      toolName: 'Bash',
      input: {},
    }]]))

    render(ChatView, { props: { agentId: 'a1', agentState: 'paused' } })

    const textarea = await screen.findByRole('textbox')
    expect((textarea as HTMLTextAreaElement).disabled).toBe(true)
    expect((textarea as HTMLTextAreaElement).placeholder).toBe('Waiting for approval...')
  })

  // Regression: a second agent's pending approval must not leak into this
  // agent's ChatView — approvals are scoped per agentId.
  it('does not show another agent\'s pending approval', async () => {
    pendingApprovals.set('a2', new SvelteMap([['tool-2', {
      toolUseId: 'tool-2',
      toolName: 'Write',
      input: {},
    }]]))

    render(ChatView, { props: { agentId: 'a1', agentState: 'paused' } })

    const textarea = await screen.findByRole('textbox')
    expect((textarea as HTMLTextAreaElement).disabled).toBe(false)
    expect((textarea as HTMLTextAreaElement).placeholder).toBe('Type a message...')
  })

  it('forwards typed messages to convoStore.sendMessage', async () => {
    mockSendMessage.mockResolvedValue(undefined)
    render(ChatView, { props: { agentId: 'a1', agentState: 'running' } })

    const textarea = (await screen.findByRole('textbox')) as HTMLTextAreaElement
    await fireEvent.input(textarea, { target: { value: 'queue me' } })
    await fireEvent.keyDown(textarea, { key: 'Enter' })

    expect(mockSendMessage).toHaveBeenCalledWith('a1', 'queue me')
  })

  it('shows the waiting placeholder until the first event arrives', async () => {
    render(ChatView, { props: { agentId: 'a1', agentState: 'running' } })

    expect(await screen.findByText('Waiting for response...')).toBeDefined()
  })
})
