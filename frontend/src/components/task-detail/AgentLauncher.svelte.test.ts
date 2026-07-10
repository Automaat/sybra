import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'

const mockStart = vi.fn()

let onlineState = true

vi.mock('../../stores/agents.svelte.js', () => ({
  agentStore: {
    start: (...args: unknown[]) => mockStart(...args),
  },
}))

vi.mock('../../stores/connection.svelte.js', () => ({
  connectionStore: {
    get online() {
      return onlineState
    },
  },
}))

const AgentLauncher = (await import('./AgentLauncher.svelte')).default

const baseTask = {
  id: 't1',
  slug: 'demo',
  title: 'X',
  status: 'todo',
  body: '',
  tags: [],
  agentMode: 'headless',
  allowedTools: [],
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
}

describe('AgentLauncher', () => {
  beforeEach(() => {
    mockStart.mockReset()
    onlineState = true
  })
  afterEach(cleanup)

  it('shows the Start agent form', () => {
    render(AgentLauncher, { props: { task: baseTask as never } })
    expect(screen.getByText('Start agent')).toBeDefined()
  })

  it('Start agent button is disabled when prompt empty', () => {
    render(AgentLauncher, { props: { task: baseTask as never } })
    const btn = screen.getByText('Start agent') as HTMLButtonElement
    expect(btn.disabled).toBe(true)
  })

  it('calls agentStore.start with prompt', async () => {
    mockStart.mockResolvedValue({ id: 'a1', state: 'running', mode: 'headless' })
    render(AgentLauncher, { props: { task: baseTask as never } })
    const ta = screen.getByPlaceholderText('Enter prompt for the agent...') as HTMLTextAreaElement
    await fireEvent.input(ta, { target: { value: 'do the thing' } })
    await fireEvent.click(screen.getByText('Start agent'))
    await waitFor(() => {
      expect(mockStart).toHaveBeenCalledWith('t1', 'headless', 'do the thing', false)
    })
  })

  it('includes task description when opted in', async () => {
    mockStart.mockResolvedValue({ id: 'a1', state: 'running', mode: 'headless' })
    render(AgentLauncher, { props: { task: baseTask as never } })
    const ta = screen.getByPlaceholderText('Enter prompt for the agent...') as HTMLTextAreaElement
    await fireEvent.input(ta, { target: { value: 'do the thing' } })
    const include = screen.getByLabelText('Include task description') as HTMLInputElement
    await fireEvent.click(include)
    await fireEvent.click(screen.getByText('Start agent'))
    await waitFor(() => {
      expect(mockStart).toHaveBeenCalledWith('t1', 'headless', 'do the thing', true)
    })
  })

  it('shows Offline indicator when connection.offline', () => {
    onlineState = false
    render(AgentLauncher, { props: { task: baseTask as never } })
    expect(screen.getByText('Offline')).toBeDefined()
  })

  it('collapses the new-run form for a plan-review task', () => {
    render(AgentLauncher, {
      props: { task: { ...baseTask, status: 'plan-review' } as never },
    })
    expect(screen.queryByText('Start agent')).toBeNull()
  })

  it('collapses the form when workflow awaits plan approval despite a desynced status', () => {
    render(AgentLauncher, {
      props: {
        task: {
          ...baseTask,
          status: 'planning',
          workflow: { currentStep: 'review_plan', state: 'waiting' },
        } as never,
      },
    })
    expect(screen.queryByText('Start agent')).toBeNull()
  })

  it('shows the form when the workflow has advanced past plan review despite a stale plan-review status', () => {
    render(AgentLauncher, {
      props: {
        task: {
          ...baseTask,
          status: 'plan-review',
          workflow: { currentStep: 'implement', state: 'running' },
        } as never,
      },
    })
    expect(screen.getByText('Start agent')).toBeDefined()
  })
})
