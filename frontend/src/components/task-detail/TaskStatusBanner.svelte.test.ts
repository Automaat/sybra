import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/svelte'
import TaskStatusBanner from './TaskStatusBanner.svelte'
import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'

function task(overrides: Record<string, unknown> = {}): Task {
  return { status: 'in-progress', statusReason: '', ...overrides } as unknown as Task
}

describe('TaskStatusBanner', () => {
  it('surfaces an awaiting sub-state with an action hint (detail >= board)', () => {
    render(TaskStatusBanner, { props: { task: task({ status: 'plan-review' }) } })
    expect(screen.getByText('Plan Review')).toBeDefined()
    expect(screen.getByText(/awaiting your approval/)).toBeDefined()
  })

  it('surfaces a quiet folded sub-state (ready-review) the dropdown would hide', () => {
    render(TaskStatusBanner, { props: { task: task({ status: 'ready-review' }) } })
    expect(screen.getByText('Ready for Review')).toBeDefined()
  })

  it('renders nothing for a plain core status with no reason', () => {
    const { container } = render(TaskStatusBanner, { props: { task: task({ status: 'in-progress' }) } })
    expect(container.querySelector('[role="status"]')).toBeNull()
  })

  it('shows the status reason when present', () => {
    render(TaskStatusBanner, {
      props: { task: task({ status: 'in-progress', statusReason: 'Waiting on upstream fix' }) },
    })
    expect(screen.getByText('Waiting on upstream fix')).toBeDefined()
  })

  it('uses the warm (attention) styling when a folded sub-state also has a reason', () => {
    const { container } = render(TaskStatusBanner, {
      props: { task: task({ status: 'ready-review', statusReason: 'CI is red' }) },
    })
    // A status_reason elevates an otherwise-quiet sub-state to the warm banner.
    expect(container.querySelector('[role="status"]')?.className).toMatch(/border-warning/)
  })
})
