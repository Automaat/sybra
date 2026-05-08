import { describe, it, expect, vi, beforeEach } from 'vitest'
import type { PaletteCtx } from './palette-commands.js'

const mockTaskList = vi.hoisted(() => ({ value: [] as any[] }))
const mockProjectList = vi.hoisted(() => ({ value: [] as any[] }))
const mockAgentList = vi.hoisted(() => ({ value: [] as any[] }))
const mockStepTexts = vi.hoisted(() => ({ value: new Map<string, string>() }))

vi.mock('../stores/tasks.svelte.js', () => ({
  taskStore: {
    get list() { return mockTaskList.value },
  },
}))

vi.mock('../stores/projects.svelte.js', () => ({
  projectStore: {
    get list() { return mockProjectList.value },
  },
}))

vi.mock('../stores/agents.svelte.js', () => ({
  agentStore: {
    get list() { return mockAgentList.value },
    get stepTexts() { return mockStepTexts.value },
  },
}))

const { buildCommands } = await import('./palette-commands.js')

function makeCtx(overrides: Partial<PaletteCtx> = {}): PaletteCtx {
  return {
    navigate: vi.fn(),
    openNewTask: vi.fn(),
    openNewProject: vi.fn(),
    openKeyboardHelp: vi.fn(),
    ...overrides,
  }
}

describe('buildCommands', () => {
  beforeEach(() => {
    mockTaskList.value = []
    mockProjectList.value = []
    mockAgentList.value = []
    mockStepTexts.value = new Map()
  })

  describe('action commands', () => {
    it('includes new-task action', () => {
      const cmds = buildCommands(makeCtx())
      const cmd = cmds.find(c => c.id === 'action:new-task')
      expect(cmd).toBeDefined()
      expect(cmd?.title).toBe('New Task')
      expect(cmd?.section).toBe('action')
      expect(cmd?.shortcut).toBe('⌘N')
    })

    it('calls openNewTask when new-task runs', () => {
      const openNewTask = vi.fn()
      const cmds = buildCommands(makeCtx({ openNewTask }))
      cmds.find(c => c.id === 'action:new-task')!.run()
      expect(openNewTask).toHaveBeenCalled()
    })

    it('includes new-project action', () => {
      const cmds = buildCommands(makeCtx())
      const cmd = cmds.find(c => c.id === 'action:new-project')
      expect(cmd).toBeDefined()
      expect(cmd?.title).toBe('New Project')
    })

    it('calls openNewProject when new-project runs', () => {
      const openNewProject = vi.fn()
      const cmds = buildCommands(makeCtx({ openNewProject }))
      cmds.find(c => c.id === 'action:new-project')!.run()
      expect(openNewProject).toHaveBeenCalled()
    })

    it('includes new-chat action that navigates to chats', () => {
      const navigate = vi.fn()
      const cmds = buildCommands(makeCtx({ navigate }))
      cmds.find(c => c.id === 'action:new-chat')!.run()
      expect(navigate).toHaveBeenCalledWith({ kind: 'chats' })
    })

    it('includes keyboard-help action', () => {
      const openKeyboardHelp = vi.fn()
      const cmds = buildCommands(makeCtx({ openKeyboardHelp }))
      const cmd = cmds.find(c => c.id === 'action:keyboard-help')!
      expect(cmd.shortcut).toBe('⌘/')
      cmd.run()
      expect(openKeyboardHelp).toHaveBeenCalled()
    })
  })

  describe('page commands', () => {
    it('includes all page commands', () => {
      const cmds = buildCommands(makeCtx())
      const pages = cmds.filter(c => c.section === 'page')
      const ids = pages.map(c => c.id)
      expect(ids).toContain('page:dashboard')
      expect(ids).toContain('page:task-list')
      expect(ids).toContain('page:project-list')
      expect(ids).toContain('page:agents')
      expect(ids).toContain('page:github')
      expect(ids).toContain('page:reviews')
      expect(ids).toContain('page:stats')
      expect(ids).toContain('page:settings')
      expect(ids).toContain('page:chats')
      expect(ids).toContain('page:workflows')
    })

    it('page shortcuts are set correctly', () => {
      const cmds = buildCommands(makeCtx())
      expect(cmds.find(c => c.id === 'page:dashboard')?.shortcut).toBe('⌘1')
      expect(cmds.find(c => c.id === 'page:task-list')?.shortcut).toBe('⌘2')
      expect(cmds.find(c => c.id === 'page:settings')?.shortcut).toBe('⌘,')
    })

    it('page command run calls navigate with correct page', () => {
      const navigate = vi.fn()
      const cmds = buildCommands(makeCtx({ navigate }))
      cmds.find(c => c.id === 'page:settings')!.run()
      expect(navigate).toHaveBeenCalledWith({ kind: 'settings' })
    })

    it('page:chats navigates to chats', () => {
      const navigate = vi.fn()
      const cmds = buildCommands(makeCtx({ navigate }))
      cmds.find(c => c.id === 'page:chats')!.run()
      expect(navigate).toHaveBeenCalledWith({ kind: 'chats' })
    })
  })

  describe('task commands', () => {
    it('returns no task commands when no tasks', () => {
      const cmds = buildCommands(makeCtx())
      expect(cmds.filter(c => c.section === 'task')).toHaveLength(0)
    })

    it('creates task command for each task', () => {
      mockTaskList.value = [
        { id: 't1', title: 'Fix auth', status: 'todo', tags: [] },
        { id: 't2', title: 'Add tests', status: 'in-progress', tags: ['backend'] },
      ]
      const cmds = buildCommands(makeCtx())
      const tasks = cmds.filter(c => c.section === 'task')
      expect(tasks).toHaveLength(2)
    })

    it('task command has correct id format', () => {
      mockTaskList.value = [{ id: 'task-abc', title: 'Test', status: 'todo', tags: [] }]
      const cmds = buildCommands(makeCtx())
      expect(cmds.find(c => c.id === 'task:task-abc')).toBeDefined()
    })

    it('task command run navigates to task-detail', () => {
      mockTaskList.value = [{ id: 'task-1', title: 'T', status: 'todo', tags: [] }]
      const navigate = vi.fn()
      const cmds = buildCommands(makeCtx({ navigate }))
      cmds.find(c => c.id === 'task:task-1')!.run()
      expect(navigate).toHaveBeenCalledWith({ kind: 'task-detail', taskId: 'task-1' })
    })

    it('task keywords include id and tags', () => {
      mockTaskList.value = [{ id: 'task-1', title: 'T', status: 'todo', tags: ['backend', 'auth'] }]
      const cmds = buildCommands(makeCtx())
      const cmd = cmds.find(c => c.id === 'task:task-1')!
      expect(cmd.keywords).toContain('task-1')
      expect(cmd.keywords).toContain('backend')
      expect(cmd.keywords).toContain('auth')
    })

    it('task subtitle is the status', () => {
      mockTaskList.value = [{ id: 't1', title: 'T', status: 'in-review', tags: [] }]
      const cmds = buildCommands(makeCtx())
      expect(cmds.find(c => c.id === 'task:t1')?.subtitle).toBe('in-review')
    })
  })

  describe('project commands', () => {
    it('returns no project commands when no projects', () => {
      const cmds = buildCommands(makeCtx())
      expect(cmds.filter(c => c.section === 'project')).toHaveLength(0)
    })

    it('creates project command with owner/repo title', () => {
      mockProjectList.value = [{ id: 'org/repo', owner: 'org', repo: 'repo' }]
      const cmds = buildCommands(makeCtx())
      const cmd = cmds.find(c => c.id === 'project:org/repo')
      expect(cmd?.title).toBe('org/repo')
    })

    it('project run navigates to project-detail', () => {
      mockProjectList.value = [{ id: 'org/repo', owner: 'org', repo: 'repo' }]
      const navigate = vi.fn()
      const cmds = buildCommands(makeCtx({ navigate }))
      cmds.find(c => c.id === 'project:org/repo')!.run()
      expect(navigate).toHaveBeenCalledWith({ kind: 'project-detail', projectId: 'org/repo' })
    })
  })

  describe('agent commands', () => {
    it('excludes stopped agents', () => {
      mockAgentList.value = [{ id: 'a1', state: 'stopped' }]
      const cmds = buildCommands(makeCtx())
      expect(cmds.filter(c => c.section === 'agent')).toHaveLength(0)
    })

    it('includes running agents', () => {
      mockAgentList.value = [{ id: 'a1', state: 'running' }]
      mockStepTexts.value = new Map([['a1', 'Running tests']])
      const cmds = buildCommands(makeCtx())
      expect(cmds.filter(c => c.section === 'agent')).toHaveLength(1)
    })

    it('includes waiting agents', () => {
      mockAgentList.value = [{ id: 'a1', state: 'waiting' }]
      const cmds = buildCommands(makeCtx())
      expect(cmds.filter(c => c.section === 'agent')).toHaveLength(1)
    })

    it('includes errored agents', () => {
      mockAgentList.value = [{ id: 'a1', state: 'errored' }]
      const cmds = buildCommands(makeCtx())
      expect(cmds.filter(c => c.section === 'agent')).toHaveLength(1)
    })

    it('uses stepTexts when available', () => {
      mockAgentList.value = [{ id: 'a1', state: 'running' }]
      mockStepTexts.value = new Map([['a1', 'Editing src/main.ts']])
      const cmds = buildCommands(makeCtx())
      expect(cmds.find(c => c.id === 'agent:a1')?.title).toBe('Editing src/main.ts')
    })

    it('falls back to agent id when no stepText', () => {
      mockAgentList.value = [{ id: 'a1', state: 'running' }]
      const cmds = buildCommands(makeCtx())
      expect(cmds.find(c => c.id === 'agent:a1')?.title).toBe('a1')
    })

    it('agent run navigates to agent-detail', () => {
      mockAgentList.value = [{ id: 'a1', state: 'running' }]
      const navigate = vi.fn()
      const cmds = buildCommands(makeCtx({ navigate }))
      cmds.find(c => c.id === 'agent:a1')!.run()
      expect(navigate).toHaveBeenCalledWith({ kind: 'agent-detail', agentId: 'a1' })
    })

    it('agent subtitle is the state', () => {
      mockAgentList.value = [{ id: 'a1', state: 'running' }]
      const cmds = buildCommands(makeCtx())
      expect(cmds.find(c => c.id === 'agent:a1')?.subtitle).toBe('running')
    })

    it('excludes idle agents', () => {
      mockAgentList.value = [{ id: 'a1', state: 'idle' }]
      const cmds = buildCommands(makeCtx())
      expect(cmds.filter(c => c.section === 'agent')).toHaveLength(0)
    })
  })

  describe('command ordering', () => {
    it('actions appear before pages', () => {
      const cmds = buildCommands(makeCtx())
      const firstAction = cmds.findIndex(c => c.section === 'action')
      const firstPage = cmds.findIndex(c => c.section === 'page')
      expect(firstAction).toBeLessThan(firstPage)
    })

    it('pages appear before tasks', () => {
      mockTaskList.value = [{ id: 't1', title: 'T', status: 'todo', tags: [] }]
      const cmds = buildCommands(makeCtx())
      const lastPage = cmds.map(c => c.section).lastIndexOf('page')
      const firstTask = cmds.findIndex(c => c.section === 'task')
      expect(lastPage).toBeLessThan(firstTask)
    })
  })
})
