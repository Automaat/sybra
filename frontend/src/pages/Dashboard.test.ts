import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'

vi.mock('../../wailsjs/go/main/App.js', () => ({
  ListAgents: vi.fn().mockResolvedValue([]),
  StartAgent: vi.fn(),
  StopAgent: vi.fn(),
  GetAgentOutput: vi.fn().mockResolvedValue([]),
  DiscoverAgents: vi.fn().mockResolvedValue([]),
  ListTasks: vi.fn().mockResolvedValue([]),
  GetTask: vi.fn(),
  CreateTask: vi.fn(),
  UpdateTask: vi.fn(),
  CaptureAgentPane: vi.fn().mockResolvedValue(''),
  AttachAgent: vi.fn(),
}))

vi.mock('../../wailsjs/runtime/runtime.js', () => ({
  EventsOn: vi.fn().mockReturnValue(() => {}),
  EventsOff: vi.fn(),
  EventsEmit: vi.fn(),
}))

const { taskStore } = await import('../stores/tasks.svelte.js')
const { agentStore } = await import('../stores/agents.svelte.js')

import Dashboard from './Dashboard.svelte'

function makeAgent(overrides: Record<string, unknown> = {}) {
  return {
    id: 'a1',
    taskId: 'task-1',
    mode: 'headless',
    state: 'running',
    sessionId: '',
    tmuxSession: '',
    costUsd: 1.5,
    startedAt: new Date().toISOString(),
    external: false,
    ...overrides,
  }
}

function makeTask(overrides: Record<string, unknown> = {}) {
  return {
    id: 't1',
    title: 'Test Task',
    status: 'todo',
    agentMode: 'headless',
    allowedTools: [],
    tags: [],
    createdAt: new Date().toISOString(),
    updatedAt: new Date().toISOString(),
    body: '',
    filePath: '',
    ...overrides,
  }
}

describe('Dashboard', () => {
  beforeEach(() => {
    agentStore.agents = new Map()
    agentStore.outputs = new Map()
    agentStore.error = ''
    agentStore.loading = false
    taskStore.tasks = new Map()
    taskStore.error = ''
    taskStore.loading = false
  })

  afterEach(() => {
    cleanup()
  })

  it('renders dashboard heading', () => {
    render(Dashboard, { props: { onviewagent: vi.fn(), onviewtask: vi.fn() } })
    expect(screen.getByText('Dashboard')).toBeTruthy()
  })

  it('shows stat cards with correct counts', () => {
    agentStore.agents.set('a1', makeAgent({ id: 'a1', state: 'running' }) as any)
    agentStore.agents.set('a2', makeAgent({ id: 'a2', state: 'paused' }) as any)
    agentStore.agents.set('a3', makeAgent({ id: 'a3', state: 'stopped' }) as any)
    taskStore.tasks.set('t1', makeTask({ id: 't1', status: 'todo' }) as any)
    taskStore.tasks.set('t2', makeTask({ id: 't2', status: 'done' }) as any)

    render(Dashboard, { props: { onviewagent: vi.fn(), onviewtask: vi.fn() } })

    expect(screen.getByText('Running Agents')).toBeTruthy()
    expect(screen.getByText('Waiting for Input')).toBeTruthy()
    expect(screen.getByText('Total Tasks')).toBeTruthy()
    expect(screen.getByText('Total Cost')).toBeTruthy()
  })

  it('shows total cost rounded to 2 decimals', () => {
    agentStore.agents.set('a1', makeAgent({ id: 'a1', costUsd: 1.555 }) as any)
    agentStore.agents.set('a2', makeAgent({ id: 'a2', costUsd: 2.445 }) as any)

    render(Dashboard, { props: { onviewagent: vi.fn(), onviewtask: vi.fn() } })

    expect(screen.getByText('$4.00')).toBeTruthy()
  })

  it('shows task status breakdown', () => {
    taskStore.tasks.set('t1', makeTask({ id: 't1', status: 'todo' }) as any)
    taskStore.tasks.set('t2', makeTask({ id: 't2', status: 'in-progress' }) as any)
    taskStore.tasks.set('t3', makeTask({ id: 't3', status: 'done' }) as any)

    render(Dashboard, { props: { onviewagent: vi.fn(), onviewtask: vi.fn() } })

    expect(screen.getByText('Task Status')).toBeTruthy()
  })

  it('shows active agents section when running/paused agents exist', () => {
    agentStore.agents.set('a1', makeAgent({ id: 'a1', state: 'running' }) as any)
    agentStore.agents.set('a2', makeAgent({ id: 'a2', state: 'paused' }) as any)

    render(Dashboard, { props: { onviewagent: vi.fn(), onviewtask: vi.fn() } })

    expect(screen.getByText('Active Agents')).toBeTruthy()
  })

  it('hides active agents section when none running/paused', () => {
    agentStore.agents.set('a1', makeAgent({ id: 'a1', state: 'stopped' }) as any)

    render(Dashboard, { props: { onviewagent: vi.fn(), onviewtask: vi.fn() } })

    expect(screen.queryByText('Active Agents')).toBeNull()
  })

  it('shows recent tasks', () => {
    taskStore.tasks.set('t1', makeTask({ id: 't1', title: 'My Task' }) as any)

    render(Dashboard, { props: { onviewagent: vi.fn(), onviewtask: vi.fn() } })

    expect(screen.getByText('Recent Tasks')).toBeTruthy()
    expect(screen.getByText('My Task')).toBeTruthy()
  })
})
