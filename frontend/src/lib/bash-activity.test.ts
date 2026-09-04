import { describe, it, expect } from 'vitest'
import { extractBashActivity, stripAnsi, truncateOutput } from './bash-activity.js'
import type { TimestampedStreamEvent } from './timeline.js'
import type { StreamEvent } from '../../bindings/github.com/Automaat/sybra/internal/agent/models.js'

function makeTSE(content: string, type = 'assistant', ts?: Date): TimestampedStreamEvent {
  return {
    event: { type, content } as StreamEvent,
    receivedAt: ts ?? new Date('2024-01-01T10:00:00Z'),
  }
}

describe('extractBashActivity', () => {
  it('returns empty for no events', () => {
    expect(extractBashActivity([])).toEqual([])
  })

  it('parses [Bash] lines from stream events', () => {
    const events = [makeTSE('[Bash] npm install')]
    const result = extractBashActivity(events)
    expect(result).toHaveLength(1)
    expect(result[0].command).toBe('npm install')
    expect(result[0].status).toBe('done')
    expect(result[0].output).toBe('')
  })

  it('ignores non-assistant event types', () => {
    const events = [makeTSE('[Bash] npm test', 'tool_result')]
    expect(extractBashActivity(events)).toHaveLength(0)
  })

  it('ignores lines without [Bash] prefix', () => {
    const events = [makeTSE('Running tests\nSome output')]
    expect(extractBashActivity(events)).toHaveLength(0)
  })

  it('parses multiple [Bash] lines from one event', () => {
    const events = [makeTSE('[Bash] go build\n[Bash] go test')]
    const result = extractBashActivity(events)
    expect(result).toHaveLength(2)
    expect(result[0].command).toBe('go build')
    expect(result[1].command).toBe('go test')
  })

  it('generates unique ids', () => {
    const ts = new Date('2024-01-01T10:00:00Z')
    const events = [makeTSE('[Bash] cmd1\n[Bash] cmd2', 'assistant', ts)]
    const result = extractBashActivity(events)
    expect(result[0].id).not.toBe(result[1].id)
  })
})

describe('stripAnsi', () => {
  it('strips color codes', () => {
    expect(stripAnsi('\x1b[32mgreen\x1b[0m')).toBe('green')
  })

  it('strips cursor movement', () => {
    expect(stripAnsi('\x1b[2Ktext')).toBe('text')
  })

  it('passes plain text through', () => {
    expect(stripAnsi('plain text')).toBe('plain text')
  })

  it('handles empty string', () => {
    expect(stripAnsi('')).toBe('')
  })
})

describe('truncateOutput', () => {
  it('passes through short text', () => {
    expect(truncateOutput('short')).toBe('short')
  })

  it('truncates at maxBytes', () => {
    const big = 'x'.repeat(200)
    const result = truncateOutput(big, 100)
    expect(result).toHaveLength(100 + '\n…truncated'.length)
    expect(result.endsWith('\n…truncated')).toBe(true)
  })

  it('does not truncate text exactly at limit', () => {
    const text = 'a'.repeat(100)
    expect(truncateOutput(text, 100)).toBe(text)
  })
})
