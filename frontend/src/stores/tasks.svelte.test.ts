import { describe, it, expect, vi, beforeEach } from 'vitest'
import { Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'

const mockListTasks = vi.fn()
const mockGetTask = vi.fn()
const mockCreateTask = vi.fn()
const mockUpdateTask = vi.fn()
const mockDeleteTask = vi.fn()
const mockApprovePlan = vi.fn()
const mockRejectPlan = vi.fn()
const mockSendPlanMessage = vi.fn()
const mockHasLivePlanAgent = vi.fn()
const mockApproveProposal = vi.fn()
const mockRejectProposal = vi.fn()

vi.mock('$lib/api', () => ({
  ListTasks: (...args: unknown[]) => mockListTasks(...args),
  GetTask: (...args: unknown[]) => mockGetTask(...args),
  CreateTask: (...args: unknown[]) => mockCreateTask(...args),
  UpdateTask: (...args: unknown[]) => mockUpdateTask(...args),
  DeleteTask: (...args: unknown[]) => mockDeleteTask(...args),
  ApprovePlan: (...args: unknown[]) => mockApprovePlan(...args),
  RejectPlan: (...args: unknown[]) => mockRejectPlan(...args),
  SendPlanMessage: (...args: unknown[]) => mockSendPlanMessage(...args),
  HasLivePlanAgent: (...args: unknown[]) => mockHasLivePlanAgent(...args),
  ApproveProposal: (...args: unknown[]) => mockApproveProposal(...args),
  RejectProposal: (...args: unknown[]) => mockRejectProposal(...args),
}))

const { taskStore } = await import('./tasks.svelte.js')

function makeTask(overrides: Record<string, unknown> = {}): Task {
  return Task.createFrom({
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

    it('runs a trailing fetch when throttled instead of dropping the load', async () => {
      vi.useFakeTimers()
      try {
        mockListTasks.mockResolvedValue([makeTask({ id: 't1' })])
        await taskStore.load()
        expect(mockListTasks).toHaveBeenCalledTimes(1)

        // A load within the 500ms window is throttled — but a live event
        // (task:created/updated/deleted) must not be lost. The throttled call
        // schedules a trailing fetch rather than returning empty-handed.
        mockListTasks.mockResolvedValue([makeTask({ id: 't1' }), makeTask({ id: 't2' })])
        await taskStore.load()
        expect(mockListTasks).toHaveBeenCalledTimes(1)

        await vi.advanceTimersByTimeAsync(500)
        expect(mockListTasks).toHaveBeenCalledTimes(2)
        expect(taskStore.tasks.size).toBe(2)
      } finally {
        vi.useRealTimers()
      }
    })

    it('coalesces a burst of throttled loads into one trailing fetch', async () => {
      vi.useFakeTimers()
      try {
        mockListTasks.mockResolvedValue([makeTask({ id: 't1' })])
        await taskStore.load()
        expect(mockListTasks).toHaveBeenCalledTimes(1)

        await taskStore.load()
        await taskStore.load()
        await taskStore.load()

        await vi.advanceTimersByTimeAsync(500)
        expect(mockListTasks).toHaveBeenCalledTimes(2)
      } finally {
        vi.useRealTimers()
      }
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

  describe('remove', () => {
    it('deletes task and removes from map', async () => {
      taskStore.tasks.set('t1', makeTask({ id: 't1' }))
      mockDeleteTask.mockResolvedValue(undefined)

      await taskStore.remove('t1')

      expect(mockDeleteTask).toHaveBeenCalledWith('t1')
      expect(taskStore.tasks.has('t1')).toBe(false)
    })
  })

  describe('approvePlan', () => {
    it('calls ApprovePlan and updates task in map', async () => {
      taskStore.tasks.set('t1', makeTask({ id: 't1', status: 'in-review' }))
      const approved = makeTask({ id: 't1', status: 'in-progress' })
      mockApprovePlan.mockResolvedValue(approved)

      const result = await taskStore.approvePlan('t1')

      expect(mockApprovePlan).toHaveBeenCalledWith('t1')
      expect(result.status).toBe('in-progress')
      expect(taskStore.tasks.get('t1')!.status).toBe('in-progress')
    })
  })

  describe('rejectPlan', () => {
    it('calls RejectPlan with feedback and updates task', async () => {
      taskStore.tasks.set('t1', makeTask({ id: 't1' }))
      const rejected = makeTask({ id: 't1', status: 'todo' })
      mockRejectPlan.mockResolvedValue(rejected)

      const result = await taskStore.rejectPlan('t1', 'needs work')

      expect(mockRejectPlan).toHaveBeenCalledWith('t1', 'needs work')
      expect(result.id).toBe('t1')
      expect(taskStore.tasks.get('t1')).toBeDefined()
    })
  })

  describe('approveProposal', () => {
    it('calls ApproveProposal and updates task in map', async () => {
      taskStore.tasks.set('t1', makeTask({ id: 't1', status: 'human-required' }))
      const approved = makeTask({ id: 't1', status: 'todo' })
      mockApproveProposal.mockResolvedValue(approved)

      const result = await taskStore.approveProposal('t1')

      expect(mockApproveProposal).toHaveBeenCalledWith('t1')
      expect(result.status).toBe('todo')
      expect(taskStore.tasks.get('t1')!.status).toBe('todo')
    })
  })

  describe('rejectProposal', () => {
    it('calls RejectProposal with feedback and updates task', async () => {
      taskStore.tasks.set('t1', makeTask({ id: 't1', status: 'human-required' }))
      const rejected = makeTask({ id: 't1', status: 'cancelled' })
      mockRejectProposal.mockResolvedValue(rejected)

      const result = await taskStore.rejectProposal('t1', 'not worth it')

      expect(mockRejectProposal).toHaveBeenCalledWith('t1', 'not worth it')
      expect(result.status).toBe('cancelled')
      expect(taskStore.tasks.get('t1')!.status).toBe('cancelled')
    })
  })

  describe('sendPlanMessage', () => {
    it('calls SendPlanMessage', async () => {
      mockSendPlanMessage.mockResolvedValue(undefined)

      await taskStore.sendPlanMessage('t1', 'please adjust')

      expect(mockSendPlanMessage).toHaveBeenCalledWith('t1', 'please adjust')
    })
  })

  describe('hasLivePlanAgent', () => {
    it('returns true when plan agent is live', async () => {
      mockHasLivePlanAgent.mockResolvedValue(true)

      const result = await taskStore.hasLivePlanAgent('t1')

      expect(result).toBe(true)
    })

    it('returns false when no plan agent', async () => {
      mockHasLivePlanAgent.mockResolvedValue(false)

      const result = await taskStore.hasLivePlanAgent('t1')

      expect(result).toBe(false)
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

  describe('tasksNeedingPlanApproval', () => {
    it('includes a task whose workflow is waiting on review_plan even if status desynced away from plan-review', () => {
      taskStore.tasks.set(
        't1',
        makeTask({
          id: 't1',
          status: 'planning',
          workflow: { currentStep: 'review_plan', state: 'waiting' },
        }),
      )
      taskStore.tasks.set('t2', makeTask({ id: 't2', status: 'todo' }))

      const result = taskStore.tasksNeedingPlanApproval()

      expect(result).toHaveLength(1)
      expect(result[0].id).toBe('t1')
    })

    it('excludes a task with a stale plan-review status once its workflow has moved on', () => {
      taskStore.tasks.set(
        't1',
        makeTask({
          id: 't1',
          status: 'plan-review',
          workflow: { currentStep: 'implement', state: 'running' },
        }),
      )

      expect(taskStore.tasksNeedingPlanApproval()).toHaveLength(0)
    })
  })
})
