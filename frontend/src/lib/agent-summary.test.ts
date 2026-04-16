import { describe, it, expect } from 'vitest'
import { summarizeAgent } from './agent-summary.js'
import type { TimestampedStreamEvent } from './timeline.js'
import type { agent } from '../../wailsjs/go/models.js'

function makeStreamEvent(type: string, content?: string): TimestampedStreamEvent {
  return {
    event: { type, content } as agent.StreamEvent,
    receivedAt: new Date(),
  }
}

function makeConvoEvent(overrides: Partial<agent.ConvoEvent>): agent.ConvoEvent {
  return {
    type: 'assistant',
    timestamp: new Date().toISOString(),
    toolUses: [],
    toolResults: [],
    ...overrides,
  } as agent.ConvoEvent
}

describe('summarizeAgent', () => {
  it('returns zero summary for empty inputs', () => {
    const result = summarizeAgent([], [])
    expect(result.filesEdited).toEqual([])
    expect(result.commandsRun).toBe(0)
    expect(result.toolUseCount).toBe(0)
    expect(result.assistantMessageCount).toBe(0)
    expect(result.finalMessage).toBe('')
  })

  describe('stream events (headless mode)', () => {
    it('counts assistant messages', () => {
      const events = [
        makeStreamEvent('assistant', 'Working on it'),
        makeStreamEvent('assistant', 'Done'),
      ]
      const result = summarizeAgent(events, [])
      expect(result.assistantMessageCount).toBe(2)
    })

    it('extracts files from [Edit] lines', () => {
      const events = [
        makeStreamEvent('assistant', '[Edit] src/foo.ts\n[Write] src/bar.ts'),
      ]
      const result = summarizeAgent(events, [])
      expect(result.filesEdited).toContain('src/foo.ts')
      expect(result.filesEdited).toContain('src/bar.ts')
    })

    it('counts Bash tool uses', () => {
      const events = [
        makeStreamEvent('assistant', '[Bash] npm test\n[Bash] go build'),
      ]
      const result = summarizeAgent(events, [])
      expect(result.commandsRun).toBe(2)
    })

    it('deduplicates file paths', () => {
      const events = [
        makeStreamEvent('assistant', '[Edit] src/foo.ts'),
        makeStreamEvent('assistant', '[Edit] src/foo.ts'),
      ]
      const result = summarizeAgent(events, [])
      expect(result.filesEdited).toEqual(['src/foo.ts'])
    })

    it('uses result event content as finalMessage', () => {
      const events = [
        makeStreamEvent('assistant', 'Working...'),
        makeStreamEvent('result', 'Task completed successfully'),
      ]
      const result = summarizeAgent(events, [])
      expect(result.finalMessage).toBe('Task completed successfully')
    })

    it('ignores lines without [ToolName] prefix', () => {
      const events = [
        makeStreamEvent('assistant', 'I will now edit the file.\n[Edit] src/foo.ts\nDone.'),
      ]
      const result = summarizeAgent(events, [])
      expect(result.filesEdited).toEqual(['src/foo.ts'])
      expect(result.toolUseCount).toBe(1)
    })
  })

  describe('convo events (interactive mode)', () => {
    it('extracts files from structured toolUses', () => {
      const convoEvents = [
        makeConvoEvent({
          type: 'assistant',
          toolUses: [
            { id: '1', name: 'Edit', input: { file_path: 'src/app.ts' } },
            { id: '2', name: 'Write', input: { file_path: 'src/new.ts' } },
          ] as agent.ToolUseBlock[],
        }),
      ]
      const result = summarizeAgent([], convoEvents)
      expect(result.filesEdited).toContain('src/app.ts')
      expect(result.filesEdited).toContain('src/new.ts')
    })

    it('counts Bash tool uses', () => {
      const convoEvents = [
        makeConvoEvent({
          type: 'assistant',
          toolUses: [
            { id: '1', name: 'Bash', input: { command: 'npm test' } },
          ] as agent.ToolUseBlock[],
        }),
      ]
      const result = summarizeAgent([], convoEvents)
      expect(result.commandsRun).toBe(1)
    })

    it('uses assistant text as finalMessage', () => {
      const convoEvents = [
        makeConvoEvent({ type: 'assistant', text: 'First message' }),
        makeConvoEvent({ type: 'assistant', text: 'Final message' }),
      ]
      const result = summarizeAgent([], convoEvents)
      expect(result.finalMessage).toBe('Final message')
    })

    it('uses result event text as finalMessage', () => {
      const convoEvents = [
        makeConvoEvent({ type: 'assistant', text: 'Working' }),
        makeConvoEvent({ type: 'result', text: 'Completed' }),
      ]
      const result = summarizeAgent([], convoEvents)
      expect(result.finalMessage).toBe('Completed')
    })

    it('ignores tools without file_path', () => {
      const convoEvents = [
        makeConvoEvent({
          type: 'assistant',
          toolUses: [
            { id: '1', name: 'Edit', input: {} },
          ] as agent.ToolUseBlock[],
        }),
      ]
      const result = summarizeAgent([], convoEvents)
      expect(result.filesEdited).toEqual([])
    })
  })

  it('merges both stream and convo events', () => {
    const stream = [makeStreamEvent('assistant', '[Edit] stream-file.ts')]
    const convo = [
      makeConvoEvent({
        toolUses: [{ id: '1', name: 'Edit', input: { file_path: 'convo-file.ts' } }] as agent.ToolUseBlock[],
      }),
    ]
    const result = summarizeAgent(stream, convo)
    expect(result.filesEdited).toContain('stream-file.ts')
    expect(result.filesEdited).toContain('convo-file.ts')
  })
})
