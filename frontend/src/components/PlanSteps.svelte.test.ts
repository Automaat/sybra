import { describe, it, expect, afterEach } from 'vitest'
import { render, screen, fireEvent, cleanup } from '@testing-library/svelte'
import PlanSteps from './PlanSteps.svelte'
import type { PlanStep } from '$lib/plan-steps.js'

function makeStep(content: string, status: 'pending' | 'in_progress' | 'completed' = 'pending'): PlanStep {
  return { content, status }
}

describe('PlanSteps', () => {
  afterEach(() => cleanup())

  it('renders plan steps', () => {
    render(PlanSteps, { props: { steps: [makeStep('Write tests'), makeStep('Deploy')] } })
    expect(screen.getByText('Write tests')).toBeDefined()
    expect(screen.getByText('Deploy')).toBeDefined()
  })

  it('shows completed count out of total', () => {
    const steps = [makeStep('s1', 'completed'), makeStep('s2', 'pending'), makeStep('s3', 'completed')]
    render(PlanSteps, { props: { steps } })
    expect(screen.getByText('2/3')).toBeDefined()
  })

  it('shows 0/N when no steps completed', () => {
    const steps = [makeStep('s1'), makeStep('s2')]
    render(PlanSteps, { props: { steps } })
    expect(screen.getByText('0/2')).toBeDefined()
  })

  it('toggles collapsed state when header clicked', async () => {
    render(PlanSteps, { props: { steps: [makeStep('step one')] } })
    expect(screen.getByText('step one')).toBeDefined()
    await fireEvent.click(screen.getByText('Plan'))
    expect(screen.queryByText('step one')).toBeNull()
  })

  it('re-expands when header clicked again', async () => {
    render(PlanSteps, { props: { steps: [makeStep('visible step')] } })
    await fireEvent.click(screen.getByText('Plan'))
    await fireEvent.click(screen.getByText('Plan'))
    expect(screen.getByText('visible step')).toBeDefined()
  })

  it('starts collapsed when collapsed=true', () => {
    render(PlanSteps, { props: { steps: [makeStep('hidden step')], collapsed: true } })
    expect(screen.queryByText('hidden step')).toBeNull()
  })

  it('renders in_progress step', () => {
    render(PlanSteps, { props: { steps: [makeStep('Running', 'in_progress')] } })
    expect(screen.getByText('Running')).toBeDefined()
  })

  it('renders completed step with strikethrough text class', () => {
    const { container } = render(PlanSteps, { props: { steps: [makeStep('Done step', 'completed')] } })
    const stepText = container.querySelector('.line-through')
    expect(stepText).toBeDefined()
    expect(stepText?.textContent?.trim()).toBe('Done step')
  })

  it('renders Plan heading', () => {
    render(PlanSteps, { props: { steps: [] } })
    expect(screen.getByText('Plan')).toBeDefined()
  })

  it('handles empty steps list', () => {
    render(PlanSteps, { props: { steps: [] } })
    expect(screen.getByText('0/0')).toBeDefined()
  })
})
