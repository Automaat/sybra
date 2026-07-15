import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/svelte'
import StatusBadge from './StatusBadge.svelte'

describe('StatusBadge', () => {
  it('renders known status label', () => {
    render(StatusBadge, { props: { status: 'todo' } })
    expect(screen.getByText('Todo')).toBeDefined()
  })

  it('renders human-required status', () => {
    render(StatusBadge, { props: { status: 'human-required' } })
    expect(screen.getByText('Human Required')).toBeDefined()
  })

  it('renders unknown status as-is', () => {
    render(StatusBadge, { props: { status: 'custom' } })
    expect(screen.getByText('custom')).toBeDefined()
  })
})
