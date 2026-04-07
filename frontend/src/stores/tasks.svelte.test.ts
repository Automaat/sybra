import { describe, it, expect, vi, beforeEach } from 'vitest'
import { task } from '../../wailsjs/go/models.js'

const mockListTasks = vi.fn()
const mockGetTask = vi.fn()
const mockCreateTask = vi.fn()
const mockUpdateTask = vi.fn()

vi.mock('../../wailsjs/go/main/TaskService.js', () => ({
  ListTasks: (...args: unknown[]) => mockListTasks(...args),
  GetTask: (...args: unknown[]) => mockGetTask(...args),
  CreateTask: (...args: unknown[]) => mockCreateTask(...args),
  UpdateTask: (...args: unknown[]) => mockUpdateTask(...args),
}))

vi.mock('../../wailsjs/go/main/PlanningService.js', () => ({
  ApprovePlan: vi.fn(),
  RejectPlan: vi.fn(),
  SendPlanMessage: vi.fn(),
  HasLivePlanAgent: vi.fn(),
}))

const { taskStore } = await import('./tasks.svelte.js')

function makeTask(overrides: Partial<task.Task> = {}): task.Task {
  return task.Task.createFrom({
    id: 'task-1',
    slug: '',
    title: 'Test task',
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
    createdAt: '2026-04-01T00:00:00Z',
    updatedAt: '2026-04-01T00:00:00Z',
    body: '## Description\nTest',
    filePath: '',
    ...overrides,
  })
}

describe('TaskStore', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    taskStore.tasks = new Map()
    taskStore.error = ''
    taskStore.loading = false
  })

  describe('load', () => {
    it('fetches tasks from backend', async () => {
      const tasks = [makeTask({ id: 't1' }), makeTask({ id: 't2' })]
      mockListTasks.mockResolvedValue(tasks)

      await taskStore.load()

      expect(mockListTasks).toHaveBeenCalled()
      expect(taskStore.tasks.size).toBe(2)
      expect(taskStore.tasks.get('t1')).toBeDefined()
      expect(taskStore.tasks.get('t2')).toBeDefined()
    })

    it('handles null result', async () => {
      mockListTasks.mockResolvedValue(null)

      await taskStore.load()

      expect(taskStore.tasks.size).toBe(0)
      expect(taskStore.error).toBe('')
    })

    it('sets error on failure', async () => {
      mockListTasks.mockRejectedValue(new Error('connection failed'))

      await taskStore.load()

      expect(taskStore.error).toBe('Error: connection failed')
    })

    it('sets loading flag', async () => {
      mockListTasks.mockResolvedValue([])

      const promise = taskStore.load()
      expect(taskStore.loading).toBe(true)
      await promise
      expect(taskStore.loading).toBe(false)
    })

    it('clears loading on error', async () => {
      mockListTasks.mockRejectedValue(new Error('fail'))

      await taskStore.load()

      expect(taskStore.loading).toBe(false)
    })
  })

  describe('get', () => {
    it('fetches single task and updates map', async () => {
      const t = makeTask({ id: 't1', title: 'Fetched' })
      mockGetTask.mockResolvedValue(t)

      const result = await taskStore.get('t1')

      expect(mockGetTask).toHaveBeenCalledWith('t1')
      expect(result.title).toBe('Fetched')
      expect(taskStore.tasks.get('t1')).toBeDefined()
    })
  })

  describe('create', () => {
    it('creates task and adds to map', async () => {
      const t = makeTask({ id: 'new-1', title: 'New task' })
      mockCreateTask.mockResolvedValue(t)

      const result = await taskStore.create('New task', 'body', 'headless')

      expect(mockCreateTask).toHaveBeenCalledWith('New task', 'body', 'headless')
      expect(result.id).toBe('new-1')
      expect(taskStore.tasks.get('new-1')).toBeDefined()
    })
  })

  describe('update', () => {
    it('updates task and refreshes map', async () => {
      taskStore.tasks.set('t1', makeTask({ id: 't1', status: 'todo' }))
      const updated = makeTask({ id: 't1', status: 'in-progress' })
      mockUpdateTask.mockResolvedValue(updated)

      const result = await taskStore.update('t1', { status: 'in-progress' })

      expect(mockUpdateTask).toHaveBeenCalledWith('t1', { status: 'in-progress' })
      expect(result.status).toBe('in-progress')
      expect(taskStore.tasks.get('t1')!.status).toBe('in-progress')
    })
  })

  describe('list', () => {
    it('returns tasks sorted by updatedAt descending', () => {
      taskStore.tasks.set('old', makeTask({
        id: 'old',
        updatedAt: '2026-01-01T00:00:00Z',
      }))
      taskStore.tasks.set('new', makeTask({
        id: 'new',
        updatedAt: '2026-04-01T00:00:00Z',
      }))

      const list = taskStore.list
      expect(list[0].id).toBe('new')
      expect(list[1].id).toBe('old')
    })

    it('handles missing updatedAt', () => {
      taskStore.tasks.set('a', makeTask({
        id: 'a',
        updatedAt: undefined,
      }))
      taskStore.tasks.set('b', makeTask({
        id: 'b',
        updatedAt: '2026-04-01T00:00:00Z',
      }))

      const list = taskStore.list
      expect(list[0].id).toBe('b')
    })
  })

  describe('byStatus', () => {
    it('filters by status', () => {
      taskStore.tasks.set('t1', makeTask({ id: 't1', status: 'todo' }))
      taskStore.tasks.set('t2', makeTask({ id: 't2', status: 'done' }))
      taskStore.tasks.set('t3', makeTask({ id: 't3', status: 'todo' }))

      expect(taskStore.byStatus('todo')).toHaveLength(2)
      expect(taskStore.byStatus('done')).toHaveLength(1)
      expect(taskStore.byStatus('human-required')).toHaveLength(0)
    })

    it('filters human-required status', () => {
      taskStore.tasks.set('t1', makeTask({ id: 't1', status: 'human-required' }))
      taskStore.tasks.set('t2', makeTask({ id: 't2', status: 'todo' }))

      expect(taskStore.byStatus('human-required')).toHaveLength(1)
      expect(taskStore.byStatus('human-required')[0].id).toBe('t1')
    })

    it('returns all for "all" filter', () => {
      taskStore.tasks.set('t1', makeTask({ id: 't1' }))
      taskStore.tasks.set('t2', makeTask({ id: 't2' }))

      expect(taskStore.byStatus('all')).toHaveLength(2)
    })
  })
})
