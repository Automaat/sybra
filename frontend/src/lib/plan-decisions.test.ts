import { describe, expect, it } from 'vitest'
import { parsePlanDecisions } from './plan-decisions.js'

describe('parsePlanDecisions', () => {
  it('parses decision sections with options', () => {
    const parsed = parsePlanDecisions(`# Decisions

## Storage shape
Question: Which artifact should implementation agents read?
Recommended: Execution contract only

Options:
- Execution contract only - smallest prompt
- Full research bundle - more context, more noise
`)

    expect(parsed.hasOpenDecisions).toBe(true)
    expect(parsed.decisions).toHaveLength(1)
    expect(parsed.decisions[0]).toMatchObject({
      id: 'storage-shape',
      title: 'Storage shape',
      question: 'Which artifact should implementation agents read?',
      recommended: 'Execution contract only',
    })
    expect(parsed.decisions[0].options).toEqual([
      { label: 'Execution contract only', description: 'smallest prompt' },
      { label: 'Full research bundle', description: 'more context, more noise' },
    ])
  })

  it('treats no-open-decisions text as autonomous', () => {
    const parsed = parsePlanDecisions(`# Decisions

No open decisions. The recommended execution contract is fully specified.
`)

    expect(parsed.hasOpenDecisions).toBe(false)
    expect(parsed.decisions).toEqual([])
  })

  it('tolerates em dash separators from model-written briefs', () => {
    const emDash = String.fromCharCode(0x2014)
    const parsed = parsePlanDecisions(`# Decisions

## Storage shape
Question: Which artifact should implementation agents read?
Recommended: Execution contract only

Options:
- Execution contract only ${emDash} smallest prompt
`)

    expect(parsed.decisions[0].options).toEqual([
      { label: 'Execution contract only', description: 'smallest prompt' },
    ])
  })
})
