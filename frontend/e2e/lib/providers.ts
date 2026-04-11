export type ProviderSpec = {
  provider: 'claude' | 'codex'
  modelLabel: string
  expectedOptions: string[]
}

export const providerMatrix: ProviderSpec[] = [
  {
    provider: 'claude',
    modelLabel: 'Default (Sonnet)',
    expectedOptions: ['Default (Sonnet)', 'Opus', 'Sonnet', 'Haiku'],
  },
  {
    provider: 'codex',
    modelLabel: 'Default (gpt-5.4)',
    expectedOptions: ['Default (gpt-5.4)', 'GPT-5.4', 'GPT-5.4 Mini', 'GPT-5.3 Codex'],
  },
]

export function selectedProviders(): ProviderSpec[] {
  const provider = process.env.SYNAPSE_E2E_PROVIDER?.trim()
  if (!provider) return providerMatrix
  const filtered = providerMatrix.filter((spec) => spec.provider === provider)
  return filtered.length > 0 ? filtered : providerMatrix
}
