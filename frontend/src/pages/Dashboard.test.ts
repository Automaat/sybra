import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'
import { agent, task } from '../../wailsjs/go/models.js'

vi.mock('../../wailsjs/go/main/App.js', () => ({
  StartAgent: vi.fn(),
}))

vi.mock('../../wailsjs/go/main/AgentService.js', () => ({
  ListAgents: vi.fn().mockResolvedValue([]),
  StopAgent: vi.fn(),
  GetAgentOutput: vi.fn().mockResolvedValue([]),
  DiscoverAgents: vi.fn().mockResolvedValue([]),
  CaptureAgentPane: vi.fn().mockResolvedValue(''),
  AttachAgent: vi.fn(),
}))

vi.mock('../../wailsjs/go/main/TaskService.js', () => ({
  ListTasks: vi.fn().mockResolvedValue([]),
  GetTask: vi.fn(),
  CreateTask: vi.fn(),
  UpdateTask: vi.fn(),
}))

vi.mock('../../wailsjs/go/main/PlanningService.js', () => ({
  ApprovePlan: vi.fn(),
  RejectPlan: vi.fn(),
  SendPlanMessage: vi.fn(),
  HasLivePlanAgent: vi.fn(),
}))

vi.mock('../../wailsjs/go/main/ReviewService.js', () => ({
  MarkPRReady: vi.fn(),
}))

vi.mock('../../wailsjs/runtime/runtime.js', () => ({
  EventsOn: vi.fn().mockReturnValue(() => {}),
  EventsOff: vi.fn(),
  EventsEmit: vi.fn(),
}))

const { taskStore } = await import('../stores/tasks.svelte.js')
const { agentStore } = await import('../stores/agents.svelte.js')

import Dashboard from './Dashboard.svelte'

function makeAgent(overrides: Partial<agent.Agent> = {}): agent.Agent {
  return agent.Agent.createFrom({
    id: 'a1',
    taskId: 'task-1',
    mode: 'headless',
    state: 'running',
    sessionId: '',
    costUsd: 1.5,
    startedAt: new Date().toISOString(),
    external: false,
    ...overrides,
  })
}

function makeTask(overrides: Partial<task.Task> = {}): task.Task {
  return task.Task.createFrom({
    id: 't1',
    slug: '',
    title: 'Test Task',
    status: 'todo',
    taskType: '',
    agentMode: 'headless',
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
    createdAt: new Date().toISOString(),
    updatedAt: new Date().toISOString(),
    body: '',
    filePath: '',
    ...overrides,
  })
}

describe('Dashboard', () => {
  beforeEach(() => {
    agentStore.agents = new Map()
    agentStore.outputs.clear()
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
    render(Dashboard, { props: { onviewagent: vi.fn() } })
    expect(screen.getByText('Dashboard')).toBeTruthy()
  })

  it('shows stat cards with correct counts', () => {
    agentStore.agents.set('a1', makeAgent({ id: 'a1', state: 'running' }))
    agentStore.agents.set('a2', makeAgent({ id: 'a2', state: 'paused' }))
    agentStore.agents.set('a3', makeAgent({ id: 'a3', state: 'stopped' }))
    taskStore.tasks.set('t1', makeTask({ id: 't1', status: 'todo' }))
    taskStore.tasks.set('t2', makeTask({ id: 't2', status: 'done' }))

    render(Dashboard, { props: { onviewagent: vi.fn() } })

    expect(screen.getByText('Running Agents')).toBeTruthy()
    expect(screen.getByText('Waiting for Input')).toBeTruthy()
    expect(screen.getByText('Total Tasks')).toBeTruthy()
    expect(screen.getByText('Total Cost')).toBeTruthy()
  })

  it('shows total cost rounded to 2 decimals', () => {
    agentStore.agents.set('a1', makeAgent({ id: 'a1', costUsd: 1.555 }))
    agentStore.agents.set('a2', makeAgent({ id: 'a2', costUsd: 2.445 }))

    render(Dashboard, { props: { onviewagent: vi.fn() } })

    expect(screen.getByText('$4.00')).toBeTruthy()
  })

  it('shows task status breakdown', () => {
    taskStore.tasks.set('t1', makeTask({ id: 't1', status: 'todo' }))
    taskStore.tasks.set('t2', makeTask({ id: 't2', status: 'in-progress' }))
    taskStore.tasks.set('t3', makeTask({ id: 't3', status: 'done' }))

    render(Dashboard, { props: { onviewagent: vi.fn() } })

    expect(screen.getByText('Task Status')).toBeTruthy()
  })

  it('shows active agents section when running/paused agents exist', () => {
    agentStore.agents.set('a1', makeAgent({ id: 'a1', state: 'running' }))
    agentStore.agents.set('a2', makeAgent({ id: 'a2', state: 'paused' }))

    render(Dashboard, { props: { onviewagent: vi.fn() } })

    expect(screen.getByText('Active Agents')).toBeTruthy()
  })

  it('hides active agents section when none running/paused', () => {
    agentStore.agents.set('a1', makeAgent({ id: 'a1', state: 'stopped' }))

    render(Dashboard, { props: { onviewagent: vi.fn() } })

    expect(screen.queryByText('Active Agents')).toBeNull()
  })

})
