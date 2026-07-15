import { describe, expect, it } from 'vitest'
import { fallbackReasoningEffortOptions, reasoningEffortOptionsFromModels } from './codex-reasoning'

describe('reasoningEffortOptionsFromModels', () => {
  it('preserves codex-supported reasoning level order and de-dupes repeats', () => {
    const got = reasoningEffortOptionsFromModels([
      {
        slug: 'gpt-a',
        display_name: 'GPT A',
        supported_reasoning_levels: ['medium', 'high'],
      },
      {
        slug: 'gpt-b',
        display_name: 'GPT B',
        supported_reasoning_levels: ['low', 'medium', 'xhigh'],
      },
    ])

    expect(got).toEqual([
      { value: 'medium', label: 'medium' },
      { value: 'high', label: 'high' },
      { value: 'low', label: 'low' },
      { value: 'xhigh', label: 'xhigh' },
    ])
  })

  it('falls back when codex returns no supported reasoning levels', () => {
    expect(reasoningEffortOptionsFromModels([
      { slug: 'gpt-a', display_name: 'GPT A' },
    ])).toBe(fallbackReasoningEffortOptions)
  })
})
