import { describe, it, expect } from 'vitest'
import { summarizeAgent } from './agent-summary.js'
import type { TimestampedStreamEvent } from './timeline.js'
import type { StreamEvent } from '../../bindings/github.com/Automaat/sybra/internal/agent/models.js'

function makeStreamEvent(type: string, content?: string): TimestampedStreamEvent {
  return {
    event: { type, content } as StreamEvent,
    receivedAt: new Date(),
  }
}

describe('summarizeAgent', () => {
  it('returns zero summary for empty inputs', () => {
    const result = summarizeAgent([])
    expect(result.filesEdited).toEqual([])
    expect(result.commandsRun).toBe(0)
    expect(result.toolUseCount).toBe(0)
    expect(result.assistantMessageCount).toBe(0)
    expect(result.finalMessage).toBe('')
  })

  it('counts assistant messages', () => {
    const events = [
      makeStreamEvent('assistant', 'Working on it'),
      makeStreamEvent('assistant', 'Done'),
    ]
    const result = summarizeAgent(events)
    expect(result.assistantMessageCount).toBe(2)
  })

  it('extracts files from [Edit] lines', () => {
    const events = [
      makeStreamEvent('assistant', '[Edit] src/foo.ts\n[Write] src/bar.ts'),
    ]
    const result = summarizeAgent(events)
    expect(result.filesEdited).toContain('src/foo.ts')
    expect(result.filesEdited).toContain('src/bar.ts')
  })

  it('counts Bash tool uses', () => {
    const events = [
      makeStreamEvent('assistant', '[Bash] npm test\n[Bash] go build'),
    ]
    const result = summarizeAgent(events)
    expect(result.commandsRun).toBe(2)
  })

  it('deduplicates file paths', () => {
    const events = [
      makeStreamEvent('assistant', '[Edit] src/foo.ts'),
      makeStreamEvent('assistant', '[Edit] src/foo.ts'),
    ]
    const result = summarizeAgent(events)
    expect(result.filesEdited).toEqual(['src/foo.ts'])
  })

  it('uses result event content as finalMessage', () => {
    const events = [
      makeStreamEvent('assistant', 'Working...'),
      makeStreamEvent('result', 'Task completed successfully'),
    ]
    const result = summarizeAgent(events)
    expect(result.finalMessage).toBe('Task completed successfully')
  })

  it('ignores lines without [ToolName] prefix', () => {
    const events = [
      makeStreamEvent('assistant', 'I will now edit the file.\n[Edit] src/foo.ts\nDone.'),
    ]
    const result = summarizeAgent(events)
    expect(result.filesEdited).toEqual(['src/foo.ts'])
    expect(result.toolUseCount).toBe(1)
  })
})
