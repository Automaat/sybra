import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'
import { agent } from '../../wailsjs/go/models.js'
const { ConvoEvent, ToolUseBlock, ToolResultBlock } = agent

vi.mock('../lib/markdown.js', () => ({
  renderMarkdown: (text: string) => `<p>${text}</p>`,
}))
vi.mock('./DiffViewer.svelte', () => ({ default: () => {} }))

const MessageBubble = (await import('./MessageBubble.svelte')).default

function makeEvent(overrides: Partial<agent.ConvoEvent> = {}): agent.ConvoEvent {
  return ConvoEvent.createFrom({
    type: 'assistant',
    text: '',
    toolUses: [],
    toolResults: [],
    costUsd: 0,
    inputTokens: 0,
    outputTokens: 0,
    ...overrides,
  })
}

describe('MessageBubble', () => {
  afterEach(() => { cleanup() })

  it('renders user_input as right-aligned bubble', () => {
    const ev = makeEvent({ type: 'user_input', text: 'Hello agent' })
    render(MessageBubble, { props: { event: ev } })
    expect(screen.getByText('Hello agent')).toBeDefined()
  })

  it('renders assistant text as markdown', () => {
    const ev = makeEvent({ type: 'assistant', text: 'Here is the answer' })
    render(MessageBubble, { props: { event: ev } })
    expect(screen.getByText('Here is the answer')).toBeDefined()
  })

  it('renders TOOL badge for tool uses', () => {
    const ev = makeEvent({
      type: 'assistant',
      text: '',
      toolUses: [
        ToolUseBlock.createFrom({ id: 'tu1', name: 'Bash', input: { command: 'ls' } }),
      ],
    })
    render(MessageBubble, { props: { event: ev } })
    expect(screen.getByText('TOOL')).toBeDefined()
    expect(screen.getByText('Bash')).toBeDefined()
  })

  it('shows bash command in pre block', () => {
    const ev = makeEvent({
      type: 'assistant',
      toolUses: [
        ToolUseBlock.createFrom({ id: 'tu1', name: 'Bash', input: { command: 'echo hello' } }),
      ],
    })
    render(MessageBubble, { props: { event: ev } })
    expect(screen.getByText('echo hello')).toBeDefined()
  })

  it('shows file_path for Edit tool', () => {
    const ev = makeEvent({
      type: 'assistant',
      toolUses: [
        ToolUseBlock.createFrom({
          id: 'tu1',
          name: 'Edit',
          input: { file_path: 'src/foo.ts', old_string: 'old', new_string: 'new' },
        }),
      ],
    })
    render(MessageBubble, { props: { event: ev } })
    expect(screen.getByText('src/foo.ts')).toBeDefined()
  })

  it('renders tool result with success style', () => {
    const ev = makeEvent({
      type: 'user',
      toolResults: [
        ToolResultBlock.createFrom({ toolUseId: 'tu1', content: 'OK output', isError: false }),
      ],
    })
    render(MessageBubble, { props: { event: ev } })
    expect(screen.getByText('OK output')).toBeDefined()
  })

  it('renders tool result with error style', () => {
    const ev = makeEvent({
      type: 'user',
      toolResults: [
        ToolResultBlock.createFrom({ toolUseId: 'tu1', content: 'Error occurred', isError: true }),
      ],
    })
    render(MessageBubble, { props: { event: ev } })
    expect(screen.getByText('Error occurred')).toBeDefined()
  })

  it('shows DONE badge for result event', () => {
    const ev = makeEvent({ type: 'result', costUsd: 0.01 })
    render(MessageBubble, { props: { event: ev } })
    expect(screen.getByText('DONE')).toBeDefined()
  })

  it('shows cost for result event when non-zero', () => {
    const ev = makeEvent({ type: 'result', costUsd: 0.0123 })
    render(MessageBubble, { props: { event: ev } })
    expect(screen.getByText('$0.0123')).toBeDefined()
  })

  it('renders nothing for system event', () => {
    const ev = makeEvent({ type: 'system', text: 'system init' })
    const { container } = render(MessageBubble, { props: { event: ev } })
    expect(container.textContent?.trim()).toBe('')
  })

  it('shows token counts for result event', () => {
    const ev = makeEvent({ type: 'result', inputTokens: 1000, outputTokens: 500 })
    render(MessageBubble, { props: { event: ev } })
    expect(screen.getByText('1,000↓ 500↑')).toBeDefined()
  })
})
