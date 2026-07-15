import { describe, it, expect } from 'vitest'
import { extractStepText } from './step-text.js'

function makeEvent(type: string, content: string) {
  return { type, content } as any
}

describe('extractStepText', () => {
  it('returns null for result events', () => {
    expect(extractStepText(makeEvent('result', 'done'))).toBeNull()
  })

  it('returns null for assistant event with no tool-use lines', () => {
    expect(extractStepText(makeEvent('assistant', 'Thinking about the problem...'))).toBeNull()
  })

  it('returns null for empty content', () => {
    expect(extractStepText(makeEvent('assistant', ''))).toBeNull()
    expect(extractStepText(makeEvent('tool_use', ''))).toBeNull()
  })

  it('extracts tool_use content for Codex events', () => {
    expect(extractStepText(makeEvent('tool_use', 'npm install'))).toBe('npm install')
  })

  it('extracts [ToolName] line from Claude assistant event', () => {
    expect(extractStepText(makeEvent('assistant', '[Bash] npm install'))).toBe('[Bash] npm install')
  })

  it('prefers last tool-use line in multi-line assistant content', () => {
    const content = 'Some thinking\n[Read] file.go\n[Bash] go test ./...'
    expect(extractStepText(makeEvent('assistant', content))).toBe('[Bash] go test ./...')
  })

  it('ignores non-tool lines after tool-use lines', () => {
    const content = '[Bash] npm install\nsome trailing text'
    expect(extractStepText(makeEvent('assistant', content))).toBe('[Bash] npm install')
  })

  it('truncates long content to 80 chars', () => {
    const long = '[Bash] ' + 'a'.repeat(100)
    const result = extractStepText(makeEvent('assistant', long))
    expect(result).toHaveLength(81) // 80 chars + '…'
    expect(result!.endsWith('…')).toBe(true)
  })

  it('truncates long tool_use content', () => {
    const long = 'a'.repeat(100)
    const result = extractStepText(makeEvent('tool_use', long))
    expect(result).toHaveLength(81)
    expect(result!.endsWith('…')).toBe(true)
  })
})
