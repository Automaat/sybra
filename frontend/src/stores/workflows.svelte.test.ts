import { describe, it, expect, vi, beforeEach } from 'vitest'
import { workflow } from '../../wailsjs/go/models.js'

const mockListWorkflows = vi.fn()
const mockGetWorkflow = vi.fn()
const mockSaveWorkflow = vi.fn()
const mockDeleteWorkflow = vi.fn()
const mockResetBuiltin = vi.fn()

vi.mock('$lib/api', () => ({
  ListWorkflows: (...args: unknown[]) => mockListWorkflows(...args),
  GetWorkflow: (...args: unknown[]) => mockGetWorkflow(...args),
  SaveWorkflow: (...args: unknown[]) => mockSaveWorkflow(...args),
  DeleteWorkflow: (...args: unknown[]) => mockDeleteWorkflow(...args),
  ResetBuiltin: (...args: unknown[]) => mockResetBuiltin(...args),
}))

const { workflowStore } = await import('./workflows.svelte.js')

function makeDef(overrides: Partial<workflow.Definition> = {}): workflow.Definition {
  return workflow.Definition.createFrom({
    id: 'wf-1',
    name: 'my-workflow',
    description: 'Does stuff',
    trigger: {},
    steps: [],
    builtin: false,
    ...overrides,
  })
}

describe('WorkflowStore', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    workflowStore.items = new Map()
    workflowStore.error = ''
    workflowStore.loading = false
    workflowStore.stopPolling()
  })

  describe('load', () => {
    it('fetches workflows from backend', async () => {
      const defs = [makeDef({ id: 'w1' }), makeDef({ id: 'w2' })]
      mockListWorkflows.mockResolvedValue(defs)

      await workflowStore.load()

      expect(mockListWorkflows).toHaveBeenCalled()
      expect(workflowStore.items.size).toBe(2)
    })

    it('handles null result', async () => {
      mockListWorkflows.mockResolvedValue(null)

      await workflowStore.load()

      expect(workflowStore.items.size).toBe(0)
      expect(workflowStore.error).toBe('')
    })

    it('sets error on failure', async () => {
      mockListWorkflows.mockRejectedValue(new Error('load failed'))

      await workflowStore.load()

      expect(workflowStore.error).toBe('Error: load failed')
    })
  })

  describe('list', () => {
    it('sorts workflows alphabetically by name', () => {
      workflowStore.items = new Map([
        ['z', makeDef({ id: 'z', name: 'zebra' })],
        ['a', makeDef({ id: 'a', name: 'alpha' })],
        ['m', makeDef({ id: 'm', name: 'monitor' })],
      ])

      const list = workflowStore.list
      expect(list[0].name).toBe('alpha')
      expect(list[1].name).toBe('monitor')
      expect(list[2].name).toBe('zebra')
    })
  })

  describe('get', () => {
    it('fetches workflow by id and adds to map', async () => {
      const def = makeDef({ id: 'w1', name: 'fetched' })
      mockGetWorkflow.mockResolvedValue(def)

      const result = await workflowStore.get('w1')

      expect(mockGetWorkflow).toHaveBeenCalledWith('w1')
      expect(result.name).toBe('fetched')
      expect(workflowStore.items.get('w1')).toBeDefined()
    })
  })

  describe('save', () => {
    it('persists workflow and updates map', async () => {
      const def = makeDef({ id: 'w1', name: 'updated' })
      mockSaveWorkflow.mockResolvedValue(undefined)

      await workflowStore.save(def)

      expect(mockSaveWorkflow).toHaveBeenCalledWith(def)
      expect(workflowStore.items.get('w1')).toEqual(def)
    })
  })

  describe('remove', () => {
    it('deletes workflow and removes from map', async () => {
      workflowStore.items.set('w1', makeDef({ id: 'w1' }))
      mockDeleteWorkflow.mockResolvedValue(undefined)

      await workflowStore.remove('w1')

      expect(mockDeleteWorkflow).toHaveBeenCalledWith('w1')
      expect(workflowStore.items.has('w1')).toBe(false)
    })
  })

  describe('resetBuiltin', () => {
    it('calls ResetBuiltin and reloads workflows', async () => {
      const refreshed = [makeDef({ id: 'w1', name: 'default' })]
      mockResetBuiltin.mockResolvedValue(undefined)
      mockListWorkflows.mockResolvedValue(refreshed)

      await workflowStore.resetBuiltin('w1')

      expect(mockResetBuiltin).toHaveBeenCalledWith('w1')
      expect(mockListWorkflows).toHaveBeenCalled()
      expect(workflowStore.items.size).toBe(1)
    })
  })
})
