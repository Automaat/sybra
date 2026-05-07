import { describe, it, expect } from 'vitest'
import { extractBashActivity, stripAnsi, truncateOutput } from './bash-activity.js'
import type { TimestampedStreamEvent } from './timeline.js'
import type { agent } from '../../wailsjs/go/models.js'

function makeTSE(content: string, type = 'assistant', ts?: Date): TimestampedStreamEvent {
  return {
    event: { type, content } as agent.StreamEvent,
    receivedAt: ts ?? new Date('2024-01-01T10:00:00Z'),
  }
}

function makeConvoEvent(overrides: Partial<agent.ConvoEvent>): agent.ConvoEvent {
  return {
    type: 'assistant',
    timestamp: new Date('2024-01-01T10:00:00Z').toISOString(),
    toolUses: [],
    toolResults: [],
    ...overrides,
  } as agent.ConvoEvent
}

describe('extractBashActivity', () => {
  describe('interactive mode (convoEvents non-empty)', () => {
    it('returns empty for no bash tool uses', () => {
      const convo = [makeConvoEvent({ type: 'assistant', toolUses: [] })]
      expect(extractBashActivity([], convo)).toEqual([])
    })

    it('extracts matched bash tool_use/tool_result pairs', () => {
      const convo = [
        makeConvoEvent({
          type: 'assistant',
          toolUses: [{ id: 'tu1', name: 'Bash', input: { command: 'npm test' } }] as agent.ToolUseBlock[],
        }),
        makeConvoEvent({
          type: 'user',
          toolResults: [{ toolUseId: 'tu1', content: 'All tests passed', isError: false }] as agent.ToolResultBlock[],
        }),
      ]
      const result = extractBashActivity([], convo)
      expect(result).toHaveLength(1)
      expect(result[0].command).toBe('npm test')
      expect(result[0].output).toBe('All tests passed')
      expect(result[0].isError).toBe(false)
      expect(result[0].status).toBe('done')
    })

    it('returns running status for unmatched tool_uses', () => {
      const convo = [
        makeConvoEvent({
          type: 'assistant',
          toolUses: [{ id: 'tu1', name: 'Bash', input: { command: 'sleep 5' } }] as agent.ToolUseBlock[],
        }),
      ]
      const result = extractBashActivity([], convo)
      expect(result).toHaveLength(1)
      expect(result[0].status).toBe('running')
      expect(result[0].output).toBe('')
    })

    it('ignores non-bash tool uses', () => {
      const convo = [
        makeConvoEvent({
          type: 'assistant',
          toolUses: [{ id: 'tu1', name: 'Edit', input: { file_path: 'foo.ts' } }] as agent.ToolUseBlock[],
        }),
      ]
      expect(extractBashActivity([], convo)).toHaveLength(0)
    })

    it('includes cwd when present', () => {
      const convo = [
        makeConvoEvent({
          type: 'assistant',
          toolUses: [{ id: 'tu1', name: 'Bash', input: { command: 'ls', cwd: '/app' } }] as agent.ToolUseBlock[],
        }),
        makeConvoEvent({
          type: 'user',
          toolResults: [{ toolUseId: 'tu1', content: '', isError: false }] as agent.ToolResultBlock[],
        }),
      ]
      const result = extractBashActivity([], convo)
      expect(result[0].cwd).toBe('/app')
    })

    it('marks error results', () => {
      const convo = [
        makeConvoEvent({
          type: 'assistant',
          toolUses: [{ id: 'tu1', name: 'Bash', input: { command: 'bad' } }] as agent.ToolUseBlock[],
        }),
        makeConvoEvent({
          type: 'user',
          toolResults: [{ toolUseId: 'tu1', content: 'command not found', isError: true }] as agent.ToolResultBlock[],
        }),
      ]
      const result = extractBashActivity([], convo)
      expect(result[0].isError).toBe(true)
    })

    it('sorts results by timestamp', () => {
      const t1 = new Date('2024-01-01T10:00:00Z')
      const t2 = new Date('2024-01-01T10:01:00Z')
      const convo = [
        makeConvoEvent({
          type: 'assistant',
          timestamp: t2.toISOString(),
          toolUses: [{ id: 'tu2', name: 'Bash', input: { command: 'second' } }] as agent.ToolUseBlock[],
        }),
        makeConvoEvent({
          type: 'assistant',
          timestamp: t1.toISOString(),
          toolUses: [{ id: 'tu1', name: 'Bash', input: { command: 'first' } }] as agent.ToolUseBlock[],
        }),
      ]
      const result = extractBashActivity([], convo)
      expect(result[0].command).toBe('first')
      expect(result[1].command).toBe('second')
    })
  })

  describe('headless mode (no convoEvents)', () => {
    it('returns empty for no events', () => {
      expect(extractBashActivity([], [])).toEqual([])
    })

    it('parses [Bash] lines from stream events', () => {
      const events = [makeTSE('[Bash] npm install')]
      const result = extractBashActivity(events, [])
      expect(result).toHaveLength(1)
      expect(result[0].command).toBe('npm install')
      expect(result[0].status).toBe('done')
      expect(result[0].output).toBe('')
    })

    it('ignores non-assistant event types', () => {
      const events = [makeTSE('[Bash] npm test', 'tool_result')]
      expect(extractBashActivity(events, [])).toHaveLength(0)
    })

    it('ignores lines without [Bash] prefix', () => {
      const events = [makeTSE('Running tests\nSome output')]
      expect(extractBashActivity(events, [])).toHaveLength(0)
    })

    it('parses multiple [Bash] lines from one event', () => {
      const events = [makeTSE('[Bash] go build\n[Bash] go test')]
      const result = extractBashActivity(events, [])
      expect(result).toHaveLength(2)
      expect(result[0].command).toBe('go build')
      expect(result[1].command).toBe('go test')
    })

    it('generates unique ids', () => {
      const ts = new Date('2024-01-01T10:00:00Z')
      const events = [makeTSE('[Bash] cmd1\n[Bash] cmd2', 'assistant', ts)]
      const result = extractBashActivity(events, [])
      expect(result[0].id).not.toBe(result[1].id)
    })
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
