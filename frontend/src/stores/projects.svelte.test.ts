import { describe, it, expect, vi, beforeEach } from 'vitest'
import { project } from '../../wailsjs/go/models.js'

const mockListProjects = vi.fn()
const mockGetProject = vi.fn()
const mockCreateProject = vi.fn()
const mockUpdateProject = vi.fn()
const mockDeleteProject = vi.fn()

vi.mock('$lib/api', () => ({
  ListProjects: (...args: unknown[]) => mockListProjects(...args),
  GetProject: (...args: unknown[]) => mockGetProject(...args),
  CreateProject: (...args: unknown[]) => mockCreateProject(...args),
  UpdateProject: (...args: unknown[]) => mockUpdateProject(...args),
  DeleteProject: (...args: unknown[]) => mockDeleteProject(...args),
}))

const { projectStore } = await import('./projects.svelte.js')

function makeProject(overrides: Partial<project.Project> = {}): project.Project {
  return project.Project.createFrom({
    id: 'proj-1',
    name: 'my-repo',
    owner: 'org',
    repo: 'my-repo',
    url: 'https://github.com/org/my-repo',
    clonePath: '/data/clones/proj-1',
    type: 'pet',
    status: 'cloned',
    createdAt: '2026-04-01T00:00:00Z',
    updatedAt: '2026-04-01T00:00:00Z',
    ...overrides,
  })
}

describe('ProjectStore', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    projectStore.projects = new Map()
  })

  describe('load', () => {
    it('fetches projects from backend', async () => {
      const projects = [makeProject({ id: 'p1' }), makeProject({ id: 'p2' })]
      mockListProjects.mockResolvedValue(projects)

      await projectStore.load()

      expect(mockListProjects).toHaveBeenCalled()
      expect(projectStore.projects.size).toBe(2)
    })

    it('handles null result', async () => {
      mockListProjects.mockResolvedValue(null)

      await projectStore.load()

      expect(projectStore.projects.size).toBe(0)
      expect(projectStore.error).toBe('')
    })

    it('sets error on failure', async () => {
      mockListProjects.mockRejectedValue(new Error('connection failed'))

      await projectStore.load()

      expect(projectStore.error).toBe('Error: connection failed')
    })
  })

  describe('list', () => {
    it('sorts projects by createdAt descending', () => {
      projectStore.projects = new Map([
        ['old', makeProject({ id: 'old', createdAt: '2026-01-01T00:00:00Z' })],
        ['new', makeProject({ id: 'new', createdAt: '2026-04-01T00:00:00Z' })],
      ])

      const list = projectStore.list
      expect(list[0].id).toBe('new')
      expect(list[1].id).toBe('old')
    })
  })

  describe('get', () => {
    it('fetches project by id and adds to map', async () => {
      const p = makeProject({ id: 'p1', name: 'fetched' })
      mockGetProject.mockResolvedValue(p)

      const result = await projectStore.get('p1')

      expect(mockGetProject).toHaveBeenCalledWith('p1')
      expect(result.name).toBe('fetched')
      expect(projectStore.projects.get('p1')).toBeDefined()
    })
  })

  describe('create', () => {
    it('creates project and adds to map', async () => {
      const p = makeProject({ id: 'new-1' })
      mockCreateProject.mockResolvedValue(p)

      const result = await projectStore.create('https://github.com/org/repo')

      expect(mockCreateProject).toHaveBeenCalledWith('https://github.com/org/repo', 'pet')
      expect(result.id).toBe('new-1')
      expect(projectStore.projects.get('new-1')).toBeDefined()
    })

    it('accepts custom project type', async () => {
      const p = makeProject({ id: 'new-2', type: 'work' })
      mockCreateProject.mockResolvedValue(p)

      await projectStore.create('https://github.com/org/work-repo', 'work')

      expect(mockCreateProject).toHaveBeenCalledWith('https://github.com/org/work-repo', 'work')
    })
  })

  describe('update', () => {
    it('updates project type and refreshes map', async () => {
      projectStore.projects.set('p1', makeProject({ id: 'p1', type: 'pet' }))
      const updated = makeProject({ id: 'p1', type: 'work' })
      mockUpdateProject.mockResolvedValue(updated)

      const result = await projectStore.update('p1', 'work')

      expect(mockUpdateProject).toHaveBeenCalledWith('p1', 'work')
      expect(result.type).toBe('work')
      expect(projectStore.projects.get('p1')!.type).toBe('work')
    })
  })

  describe('remove', () => {
    it('deletes project and removes from map', async () => {
      projectStore.projects.set('p1', makeProject({ id: 'p1' }))
      mockDeleteProject.mockResolvedValue(undefined)

      await projectStore.remove('p1')

      expect(mockDeleteProject).toHaveBeenCalledWith('p1')
      expect(projectStore.projects.has('p1')).toBe(false)
    })
  })
})
