import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, cleanup } from '@testing-library/svelte'
import AgentCard from './AgentCard.svelte'
import { agentStore } from '../stores/agents.svelte.js'
import { taskStore } from '../stores/tasks.svelte.js'
import type { task } from '../../wailsjs/go/models.js'

// Mutable clock mock — set per test as needed; vi.hoisted ensures it is accessible
// inside the hoisted vi.mock factory
const clockMock = vi.hoisted(() => ({ now: Date.now() }))
vi.mock('$lib/clock.svelte.js', () => ({ clock: clockMock }))

function makeAgent(overrides: Record<string, unknown> = {}) {
  return {
    id: 'agent-1',
    taskId: 'task-1',
    mode: 'headless',
    state: 'running',
    sessionId: '',
    costUsd: 0.1234,
    startedAt: '2026-04-01T00:00:00Z',
    external: false,
    pid: 0,
    command: '',
    name: 'test-agent',
    project: 'sybra',
    lastEventAt: '',
    convertValues: () => {},
    ...overrides,
  }
}

function makeTask(overrides: Partial<task.Task> = {}): task.Task {
  return {
    id: 'task-1',
    title: 'Test task title',
    status: 'todo',
    slug: '',
    taskType: '',
    agentMode: '',
    allowedTools: [],
    tags: [],
    projectId: '',
    branch: '',
    prNumber: 0,
    issue: '',
    statusReason: '',
    reviewed: false,
    runRole: '',
    todoistId: '',
    agentRuns: [],
    convertValues: () => {},
    ...overrides,
  } as task.Task
}

describe('AgentCard', () => {
  afterEach(() => {
    cleanup()
    taskStore.tasks = new Map()
  })

  it('renders task title in heading when linked task is present', () => {
    taskStore.tasks = new Map([['task-1', makeTask({ title: 'Implement auth' })]])
    render(AgentCard, { props: { agent: makeAgent(), onclick: () => {} } })
    expect(screen.getByRole('heading', { level: 3 }).textContent).toBe('Implement auth')
  })

  it('renders agent project name in heading when no linked task', () => {
    render(AgentCard, { props: { agent: makeAgent(), onclick: () => {} } })
    expect(screen.getByRole('heading', { level: 3 }).textContent).toBe('sybra')
  })

  it('renders agent id as fallback when project is empty and no linked task', () => {
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

  it('renders Queued label for idle state', () => {
    render(AgentCard, { props: { agent: makeAgent({ state: 'idle' }), onclick: () => {} } })
    expect(screen.getByText('Queued')).toBeDefined()
  })

  it('renders Waiting state label for paused', () => {
    render(AgentCard, { props: { agent: makeAgent({ state: 'paused' }), onclick: () => {} } })
    expect(screen.getByText('Waiting')).toBeDefined()
  })

  it('renders Done label for stopped state', () => {
    render(AgentCard, { props: { agent: makeAgent({ state: 'stopped' }), onclick: () => {} } })
    expect(screen.getByText('Done')).toBeDefined()
  })

  it('renders Done label for unknown state', () => {
    render(AgentCard, { props: { agent: makeAgent({ state: 'crashed' }), onclick: () => {} } })
    expect(screen.getByText('Done')).toBeDefined()
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

  it('calls onclick when clicked', async () => {
    const handler = vi.fn()
    render(AgentCard, { props: { agent: makeAgent(), onclick: handler } })
    await fireEvent.click(screen.getByRole('button'))
    expect(handler).toHaveBeenCalledOnce()
  })

  describe('step text', () => {
    afterEach(() => {
      agentStore.stepTexts.delete('agent-1')
    })

    it('shows "Working..." when running with no step text', () => {
      render(AgentCard, { props: { agent: makeAgent({ state: 'running' }), onclick: () => {} } })
      expect(screen.getByText('Working...')).toBeDefined()
    })

    it('shows step text from store when running', () => {
      agentStore.setStepText('agent-1', '[Bash] npm install')
      render(AgentCard, { props: { agent: makeAgent({ state: 'running' }), onclick: () => {} } })
      expect(screen.getByText('[Bash] npm install')).toBeDefined()
    })

    it('does not show step text when not running', () => {
      agentStore.setStepText('agent-1', '[Bash] npm install')
      render(AgentCard, { props: { agent: makeAgent({ state: 'idle' }), onclick: () => {} } })
      expect(screen.queryByText('[Bash] npm install')).toBeNull()
      expect(screen.queryByText('Working...')).toBeNull()
    })
  })

  describe('elapsed time for active phases', () => {
    const START = '2026-04-01T00:00:00Z'
    const START_MS = new Date(START).getTime()

    it('shows elapsed time for running agent', () => {
      clockMock.now = START_MS + 90 * 1000
      render(AgentCard, {
        props: { agent: makeAgent({ state: 'running', startedAt: START }), onclick: () => {} },
      })
      expect(screen.getByText('1m 30s')).toBeDefined()
    })
  })

  describe('timeAgo for terminal phases', () => {
    beforeEach(() => {
      vi.useFakeTimers()
      vi.setSystemTime(new Date('2026-04-01T01:00:00Z'))
    })

    afterEach(() => {
      vi.useRealTimers()
    })

    it('returns "just now" for stopped agent started < 60s ago', () => {
      render(AgentCard, {
        props: { agent: makeAgent({ state: 'stopped', startedAt: '2026-04-01T00:59:30Z' }), onclick: () => {} },
      })
      expect(screen.getByText('just now')).toBeDefined()
    })

    it('returns minutes ago for stopped agent', () => {
      render(AgentCard, {
        props: { agent: makeAgent({ state: 'stopped', startedAt: '2026-04-01T00:55:00Z' }), onclick: () => {} },
      })
      expect(screen.getByText('5m ago')).toBeDefined()
    })

    it('returns hours ago for stopped agent', () => {
      render(AgentCard, {
        props: { agent: makeAgent({ state: 'stopped', startedAt: '2026-04-01T00:00:00Z' }), onclick: () => {} },
      })
      expect(screen.getByText('1h ago')).toBeDefined()
    })

    it('returns days ago for stopped agent', () => {
      render(AgentCard, {
        props: { agent: makeAgent({ state: 'stopped', startedAt: '2026-03-30T00:00:00Z' }), onclick: () => {} },
      })
      expect(screen.getByText('2d ago')).toBeDefined()
    })
  })
})
