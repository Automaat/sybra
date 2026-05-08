import { describe, it, expect } from 'vitest'
import { buildStreamTimeline, buildConvoTimeline } from './timeline.js'
import type { TimestampedStreamEvent } from './timeline.js'

function makeStreamEvent(type: string, overrides: Record<string, unknown> = {}): TimestampedStreamEvent {
  return {
    event: { type, content: '', ...overrides } as any,
    receivedAt: new Date('2026-01-01T00:00:00Z'),
  }
}

function makeConvoEvent(type: string, overrides: Record<string, unknown> = {}) {
  return {
    type,
    text: '',
    toolUses: [],
    toolResults: [],
    timestamp: '2026-01-01T00:00:00Z',
    ...overrides,
  } as any
}

describe('buildStreamTimeline', () => {
  it('returns empty array for no events', () => {
    expect(buildStreamTimeline([])).toEqual([])
  })

  it('preserves event count', () => {
    const events = [
      makeStreamEvent('init'),
      makeStreamEvent('assistant'),
      makeStreamEvent('result'),
    ]
    expect(buildStreamTimeline(events)).toHaveLength(3)
  })

  it('assigns sequential indices', () => {
    const events = [makeStreamEvent('init'), makeStreamEvent('assistant')]
    const timeline = buildStreamTimeline(events)
    expect(timeline[0].index).toBe(0)
    expect(timeline[1].index).toBe(1)
  })

  it('summarizes "init" event as "Session started"', () => {
    const [entry] = buildStreamTimeline([makeStreamEvent('init')])
    expect(entry.summary).toBe('Session started')
  })

  it('summarizes "assistant" event with content', () => {
    const [entry] = buildStreamTimeline([makeStreamEvent('assistant', { content: 'Hello world' })])
    expect(entry.summary).toBe('Hello world')
  })

  it('summarizes "assistant" with no content as "Assistant"', () => {
    const [entry] = buildStreamTimeline([makeStreamEvent('assistant', { content: '' })])
    expect(entry.summary).toBe('Assistant')
  })

  it('summarizes "tool_use" event with content', () => {
    const [entry] = buildStreamTimeline([makeStreamEvent('tool_use', { content: 'Bash: ls -la' })])
    expect(entry.summary).toBe('Bash: ls -la')
  })

  it('summarizes "tool_use" without content as "Tool use"', () => {
    const [entry] = buildStreamTimeline([makeStreamEvent('tool_use', { content: '' })])
    expect(entry.summary).toBe('Tool use')
  })

  it('summarizes "tool_result" with content', () => {
    const [entry] = buildStreamTimeline([makeStreamEvent('tool_result', { content: 'output text' })])
    expect(entry.summary).toContain('Result:')
  })

  it('summarizes "tool_result" without content as "Result"', () => {
    const [entry] = buildStreamTimeline([makeStreamEvent('tool_result', { content: '' })])
    expect(entry.summary).toBe('Result')
  })

  it('summarizes "result" event as "Done"', () => {
    const [entry] = buildStreamTimeline([makeStreamEvent('result', { cost_usd: undefined })])
    expect(entry.summary).toBe('Done')
  })

  it('includes cost in "result" summary when cost_usd set', () => {
    const [entry] = buildStreamTimeline([makeStreamEvent('result', { cost_usd: 0.05 })])
    expect(entry.summary).toContain('$0.05')
  })

  it('truncates long content to 60 chars with ellipsis', () => {
    const longText = 'A'.repeat(100)
    const [entry] = buildStreamTimeline([makeStreamEvent('tool_use', { content: longText })])
    expect(entry.summary.length).toBeLessThanOrEqual(65) // 60 chars + '…'
    expect(entry.summary.endsWith('…')).toBe(true)
  })

  it('uses the last non-bracket line for assistant content', () => {
    const content = 'Line 1\nLine 2\n[STATUS: done]'
    const [entry] = buildStreamTimeline([makeStreamEvent('assistant', { content })])
    expect(entry.summary).toBe('[STATUS: done]')
  })

  it('sets type from event type', () => {
    const [entry] = buildStreamTimeline([makeStreamEvent('tool_use')])
    expect(entry.type).toBe('tool_use')
  })

  it('sets timestamp from receivedAt', () => {
    const receivedAt = new Date('2026-05-01T12:00:00Z')
    const [entry] = buildStreamTimeline([{ event: { type: 'init' } as any, receivedAt }])
    expect(entry.timestamp).toEqual(receivedAt)
  })
})

describe('buildConvoTimeline', () => {
  it('returns empty array for no events', () => {
    expect(buildConvoTimeline([])).toEqual([])
  })

  it('preserves event count', () => {
    const events = [makeConvoEvent('user_input'), makeConvoEvent('assistant'), makeConvoEvent('result')]
    expect(buildConvoTimeline(events)).toHaveLength(3)
  })

  it('summarizes "user_input" with text', () => {
    const [entry] = buildConvoTimeline([makeConvoEvent('user_input', { text: 'Fix the bug' })])
    expect(entry.summary).toContain('User:')
    expect(entry.summary).toContain('Fix the bug')
  })

  it('summarizes "user_input" without text as "User input"', () => {
    const [entry] = buildConvoTimeline([makeConvoEvent('user_input', { text: '' })])
    expect(entry.summary).toBe('User input')
  })

  it('summarizes "assistant" with tool uses as tool names', () => {
    const [entry] = buildConvoTimeline([
      makeConvoEvent('assistant', { toolUses: [{ name: 'Bash' }, { name: 'Read' }] }),
    ])
    expect(entry.summary).toBe('Bash, Read')
  })

  it('summarizes "assistant" with text when no tool uses', () => {
    const [entry] = buildConvoTimeline([makeConvoEvent('assistant', { text: 'I will fix this' })])
    expect(entry.summary).toContain('I will fix this')
  })

  it('summarizes "assistant" with no content as "Assistant"', () => {
    const [entry] = buildConvoTimeline([makeConvoEvent('assistant', { text: '', toolUses: [] })])
    expect(entry.summary).toBe('Assistant')
  })

  it('summarizes "user" with error as "Result (error)"', () => {
    const [entry] = buildConvoTimeline([
      makeConvoEvent('user', { toolResults: [{ isError: true }] }),
    ])
    expect(entry.summary).toBe('Result (error)')
  })

  it('summarizes "user" without error as "Result"', () => {
    const [entry] = buildConvoTimeline([
      makeConvoEvent('user', { toolResults: [{ isError: false }] }),
    ])
    expect(entry.summary).toBe('Result')
  })

  it('summarizes "result" as "Done"', () => {
    const [entry] = buildConvoTimeline([makeConvoEvent('result', { costUsd: undefined })])
    expect(entry.summary).toBe('Done')
  })

  it('includes cost in "result" summary when costUsd set', () => {
    const [entry] = buildConvoTimeline([makeConvoEvent('result', { costUsd: 0.12 })])
    expect(entry.summary).toContain('$0.12')
  })

  it('summarizes "system" as "System"', () => {
    const [entry] = buildConvoTimeline([makeConvoEvent('system')])
    expect(entry.summary).toBe('System')
  })

  it('parses timestamp from event timestamp string', () => {
    const [entry] = buildConvoTimeline([makeConvoEvent('user_input', { timestamp: '2026-03-01T10:00:00Z' })])
    expect(entry.timestamp.getFullYear()).toBe(2026)
  })
})
