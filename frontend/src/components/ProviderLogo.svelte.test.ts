import { describe, it, expect, afterEach } from 'vitest'
import { render, cleanup } from '@testing-library/svelte'

const ProviderLogo = (await import('./ProviderLogo.svelte')).default

describe('ProviderLogo', () => {
  afterEach(() => { cleanup() })

  it('renders claude logo svg', () => {
    const { container } = render(ProviderLogo, { props: { provider: 'claude' } })
    const svg = container.querySelector('svg')
    expect(svg).toBeDefined()
    expect(svg?.getAttribute('aria-label')).toBe('Anthropic / Claude')
  })

  it('renders codex logo svg', () => {
    const { container } = render(ProviderLogo, { props: { provider: 'codex' } })
    const svg = container.querySelector('svg')
    expect(svg?.getAttribute('aria-label')).toBe('OpenAI / Codex')
  })

  it('renders generic logo for unknown provider', () => {
    const { container } = render(ProviderLogo, { props: { provider: 'unknown' } })
    const svg = container.querySelector('svg')
    expect(svg?.getAttribute('aria-label')).toBe('Agent')
  })

  it('applies custom class', () => {
    const { container } = render(ProviderLogo, { props: { provider: 'claude', class: 'h-8 w-8' } })
    const svg = container.querySelector('svg')
    expect(svg?.getAttribute('class')).toBe('h-8 w-8')
  })

  it('defaults to h-4 w-4 class', () => {
    const { container } = render(ProviderLogo, { props: { provider: 'claude' } })
    const svg = container.querySelector('svg')
    expect(svg?.getAttribute('class')).toBe('h-4 w-4')
  })
})
