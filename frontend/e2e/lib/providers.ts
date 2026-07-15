export type ProviderSpec = {
  provider: 'claude' | 'codex' | 'copilot'
  modelLabel: string
  expectedOptions: string[]
}

export const providerMatrix: ProviderSpec[] = [
  {
    provider: 'claude',
    modelLabel: 'Default (Sonnet)',
    expectedOptions: ['Default (Sonnet)', 'Opus 4.8', 'Sonnet', 'Haiku', 'Fable 5'],
  },
  {
    provider: 'codex',
    modelLabel: 'Default (gpt-5.5)',
    expectedOptions: ['Default (gpt-5.5)', 'GPT-5.4', 'GPT-5.4 Mini', 'GPT-5.3 Codex'],
  },
  {
    provider: 'copilot',
    modelLabel: 'Default (GPT-5.5)',
    // Must match GetCopilotModels() / copilotFallbackModels exactly (9 entries).
    expectedOptions: [
      'Default (GPT-5.5)',
      'GPT-5.5',
      'GPT-5.4',
      'GPT-5.4 Mini',
      'GPT-5.3 Codex',
      'Claude Opus 4.6',
      'Claude Sonnet 4.6',
      'Claude Haiku 4.5',
      'Gemini 3.1 Pro',
      'Auto',
    ],
  },
]

export function selectedProviders(): ProviderSpec[] {
  const provider = process.env.SYNAPSE_E2E_PROVIDER?.trim()
  if (!provider) return providerMatrix
  const filtered = providerMatrix.filter((spec) => spec.provider === provider)
  return filtered.length > 0 ? filtered : providerMatrix
}
