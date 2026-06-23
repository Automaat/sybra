import { describe, it, expect, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'
import AgentStateBadge from './AgentStateBadge.svelte'
import { PHASE_CONFIG } from '$lib/agent-phases.js'

describe('AgentStateBadge', () => {
  afterEach(cleanup)

  it('renders the phase label', () => {
    render(AgentStateBadge, { props: { phase: 'running' } })
    expect(screen.getByText('Running')).toBeDefined()
  })

  it('applies the shared phase badge classes', () => {
    const { container } = render(AgentStateBadge, { props: { phase: 'human-required' } })
    const pill = container.querySelector('span')
    expect(pill?.className).toContain(PHASE_CONFIG['human-required'].badgeClasses.split(' ')[0])
  })

  it('renders a leading dot for active phases', () => {
    const { container } = render(AgentStateBadge, { props: { phase: 'running' } })
    expect(container.querySelectorAll('span').length).toBe(2)
  })

  it('omits the dot for resting phases (queued, done)', () => {
    const { container } = render(AgentStateBadge, { props: { phase: 'queued' } })
    expect(container.querySelectorAll('span').length).toBe(1)
  })

  it('uses larger sizing for the md variant', () => {
    const { container } = render(AgentStateBadge, { props: { phase: 'reviewing', size: 'md' } })
    expect(container.querySelector('span')?.className).toContain('text-sm')
  })
})
