export type ModelOption = { value: string; label: string }

// Claude model aliases offered in pickers. Aliases auto-resolve to the latest
// build of each family (verified against `claude --model`, CC 2.1.191, which
// accepts 'fable'|'opus'|'sonnet' aliases and 'claude-fable-5' full names).
// `opus` resolves to Opus 4.8. There is no `claude debug models` equivalent,
// so this list is curated, not discovered. Order = display order.
export const CLAUDE_MODEL_OPTIONS: ModelOption[] = [
  { value: 'opus', label: 'Opus 4.8' },
  { value: 'sonnet', label: 'Sonnet' },
  { value: 'haiku', label: 'Haiku' },
  { value: 'fable', label: 'Fable 5' },
]
