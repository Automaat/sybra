import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, fireEvent, cleanup } from '@testing-library/svelte'
import AgentListRow from './AgentListRow.svelte'
import { taskStore } from '../stores/tasks.svelte.js'
import type { Agent } from '../../bindings/github.com/Automaat/sybra/internal/agent/models.js'
import type { Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'

const clockMock = vi.hoisted(() => ({ now: Date.now() }))
vi.mock('$lib/clock.svelte.js', () => ({ clock: clockMock }))

function makeAgent(overrides: Record<string, unknown> = {}): Agent {
  return {
    id: 'agent-1',
    taskId: 'task-1',
    mode: 'interactive',
    state: 'paused',
    sessionId: '',
    costUsd: 0.42,
    startedAt: '2026-04-01T00:00:00Z',
    external: false,
    pid: 0,
    command: '',
    name: 'review:Fix the bug',
    project: 'sybra',
    lastEventAt: '',
    convertValues: () => {},
    ...overrides,
  } as unknown as Agent
}

function makeTask(overrides: Record<string, unknown> = {}): Task {
  return {
    id: 'task-1',
    title: 'Test task',
    status: 'todo',
    convertValues: () => {},
    ...overrides,
  } as unknown as Task
}

describe('AgentListRow', () => {
  afterEach(() => {
    cleanup()
    taskStore.tasks = new Map()
  })

  it('renders the friendly task title and the state badge', () => {
    taskStore.tasks = new Map([['task-1', makeTask({ title: 'Implement auth', status: 'todo' })]])
    render(AgentListRow, { props: { agent: makeAgent({ state: 'paused' }), onclick: () => {} } })
    expect(screen.getByText('Implement auth')).toBeDefined()
    // The shared AgentStateBadge renders the phase label (paused → Waiting).
    expect(screen.getByText('Waiting')).toBeDefined()
  })

  it('renders the project chip and cost', () => {
    render(AgentListRow, { props: { agent: makeAgent(), onclick: () => {} } })
    expect(screen.getByText('sybra')).toBeDefined()
    expect(screen.getByText('$0.42')).toBeDefined()
  })

  it('strips the role prefix when there is no linked task', () => {
    render(AgentListRow, { props: { agent: makeAgent({ taskId: '' }), onclick: () => {} } })
    expect(screen.getByText('Fix the bug')).toBeDefined()
  })

  it('fires onclick when the row is clicked', async () => {
    const onclick = vi.fn()
    render(AgentListRow, { props: { agent: makeAgent(), onclick } })
    await fireEvent.click(screen.getByRole('button'))
    expect(onclick).toHaveBeenCalledOnce()
  })
})
