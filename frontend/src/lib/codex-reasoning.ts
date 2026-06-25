import { GetCodexModels } from '$lib/api'
import type { CodexModel } from '../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'

export type ReasoningEffortOption = { value: string; label: string }

export const fallbackReasoningEffortOptions: ReasoningEffortOption[] = [
  { value: 'low', label: 'low' },
  { value: 'medium', label: 'medium' },
  { value: 'high', label: 'high' },
  { value: 'xhigh', label: 'xhigh' },
]

export function reasoningEffortOptionsFromModels(models: CodexModel[] | null | undefined): ReasoningEffortOption[] {
  const seen = new Set<string>()
  const options: ReasoningEffortOption[] = []

  for (const model of models ?? []) {
    for (const level of model.supported_reasoning_levels ?? []) {
      if (seen.has(level)) continue
      seen.add(level)
      options.push({ value: level, label: level })
    }
  }

  return options.length > 0 ? options : fallbackReasoningEffortOptions
}

export async function loadReasoningEffortOptions(): Promise<ReasoningEffortOption[]> {
  try {
    return reasoningEffortOptionsFromModels(await GetCodexModels())
  } catch {
    return fallbackReasoningEffortOptions
  }
}
