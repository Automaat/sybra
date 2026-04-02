import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, cleanup } from '@testing-library/svelte'
import AgentCard from './AgentCard.svelte'

function makeAgent(overrides: Record<string, unknown> = {}) {
  return {
    id: 'agent-1',
    taskId: 'task-1',
    mode: 'headless',
    state: 'running',
    sessionId: '',
    tmuxSession: '',
    costUsd: 0.1234,
    startedAt: '2026-04-01T00:00:00Z',
    external: false,
    pid: 0,
    command: '',
    name: 'test-agent',
    project: 'synapse',
    ...overrides,
  }
}

describe('AgentCard', () => {
  afterEach(() => {
    cleanup()
  })

  it('renders agent project name in heading', () => {
    render(AgentCard, { props: { agent: makeAgent(), onclick: () => {} } })
    expect(screen.getByRole('heading', { level: 3 }).textContent).toBe('synapse')
  })

  it('renders agent id as fallback when project is empty', () => {
    render(AgentCard, { props: { agent: makeAgent({ project: '' }), onclick: () => {} } })
    expect(screen.getByRole('heading', { level: 3 }).textContent).toBe('agent-1')
  })

  it('renders agent name when present', () => {
    render(AgentCard, { props: { agent: makeAgent(), onclick: () => {} } })
    expect(screen.getAllByText('test-agent').length).toBeGreaterThanOrEqual(1)
  })

  it('renders Running state label', () => {
    render(AgentCard, { props: { agent: makeAgent({ state: 'running' }), onclick: () => {} } })
    expect(screen.getByText('Running')).toBeDefined()
  })

  it('renders Idle state label', () => {
    render(AgentCard, { props: { agent: makeAgent({ state: 'idle' }), onclick: () => {} } })
    expect(screen.getByText('Idle')).toBeDefined()
  })

  it('renders Waiting state label for paused', () => {
    render(AgentCard, { props: { agent: makeAgent({ state: 'paused' }), onclick: () => {} } })
    expect(screen.getByText('Waiting')).toBeDefined()
  })

  it('renders Stopped state label', () => {
    render(AgentCard, { props: { agent: makeAgent({ state: 'stopped' }), onclick: () => {} } })
    expect(screen.getByText('Stopped')).toBeDefined()
  })

  it('renders unknown state as-is', () => {
    render(AgentCard, { props: { agent: makeAgent({ state: 'crashed' }), onclick: () => {} } })
    expect(screen.getByText('crashed')).toBeDefined()
  })

  it('renders mode', () => {
    render(AgentCard, { props: { agent: makeAgent(), onclick: () => {} } })
    expect(screen.getAllByText('headless').length).toBeGreaterThanOrEqual(1)
  })

  it('shows external badge when external is true', () => {
    render(AgentCard, { props: { agent: makeAgent({ external: true }), onclick: () => {} } })
    expect(screen.getByText('external')).toBeDefined()
  })

  it('does not show external badge when external is false', () => {
    render(AgentCard, { props: { agent: makeAgent({ external: false }), onclick: () => {} } })
    expect(screen.queryByText('external')).toBeNull()
  })

  it('shows cost when costUsd > 0', () => {
    render(AgentCard, { props: { agent: makeAgent({ costUsd: 0.1234 }), onclick: () => {} } })
    expect(screen.getByText('$0.12')).toBeDefined()
  })

  it('does not show cost when costUsd is 0', () => {
    render(AgentCard, { props: { agent: makeAgent({ costUsd: 0 }), onclick: () => {} } })
    expect(screen.queryByText(/^\$/)).toBeNull()
  })

  it('shows taskId when present', () => {
    render(AgentCard, { props: { agent: makeAgent({ taskId: 'task-1' }), onclick: () => {} } })
    expect(screen.getByText('task: task-1')).toBeDefined()
  })

  it('calls onclick when clicked', async () => {
    const handler = vi.fn()
    render(AgentCard, { props: { agent: makeAgent(), onclick: handler } })
    await fireEvent.click(screen.getByRole('button'))
    expect(handler).toHaveBeenCalledOnce()
  })

  describe('timeAgo', () => {
    beforeEach(() => {
      vi.useFakeTimers()
      vi.setSystemTime(new Date('2026-04-01T01:00:00Z'))
    })

    afterEach(() => {
      vi.useRealTimers()
    })

    it('returns "just now" for < 60s ago', () => {
      render(AgentCard, {
        props: { agent: makeAgent({ startedAt: '2026-04-01T00:59:30Z' }), onclick: () => {} },
      })
      expect(screen.getByText('just now')).toBeDefined()
    })

    it('returns minutes ago', () => {
      render(AgentCard, {
        props: { agent: makeAgent({ startedAt: '2026-04-01T00:55:00Z' }), onclick: () => {} },
      })
      expect(screen.getByText('5m ago')).toBeDefined()
    })

    it('returns hours ago', () => {
      render(AgentCard, {
        props: { agent: makeAgent({ startedAt: '2026-04-01T00:00:00Z' }), onclick: () => {} },
      })
      expect(screen.getByText('1h ago')).toBeDefined()
    })

    it('returns days ago', () => {
      render(AgentCard, {
        props: { agent: makeAgent({ startedAt: '2026-03-30T00:00:00Z' }), onclick: () => {} },
      })
      expect(screen.getByText('2d ago')).toBeDefined()
    })
  })
})
