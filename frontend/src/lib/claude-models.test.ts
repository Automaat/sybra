import { describe, it, expect } from 'vitest'
import { CLAUDE_MODEL_OPTIONS } from './claude-models'

describe('CLAUDE_MODEL_OPTIONS', () => {
  it('contains fable entry with label Fable 5', () => {
    const entry = CLAUDE_MODEL_OPTIONS.find((o) => o.value === 'fable')
    expect(entry).toBeDefined()
    expect(entry?.label).toBe('Fable 5')
  })

  it('contains opus entry with label Opus 4.8', () => {
    const entry = CLAUDE_MODEL_OPTIONS.find((o) => o.value === 'opus')
    expect(entry).toBeDefined()
    expect(entry?.label).toBe('Opus 4.8')
  })

  it('all values match safeArgRe pattern', () => {
    const safeRe = /^[a-z0-9-]+$/
    for (const option of CLAUDE_MODEL_OPTIONS) {
      expect(option.value).toMatch(safeRe)
    }
  })
})
