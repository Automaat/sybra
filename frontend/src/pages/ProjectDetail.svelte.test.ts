import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'

const mockGet = vi.fn()
const mockRemove = vi.fn()
const mockUpdate = vi.fn()

const mockTaskList: any[] = []

vi.mock('../stores/projects.svelte.js', () => ({
  projectStore: {
    get: (...args: unknown[]) => mockGet(...args),
    remove: (...args: unknown[]) => mockRemove(...args),
    update: (...args: unknown[]) => mockUpdate(...args),
  },
}))

vi.mock('../stores/tasks.svelte.js', () => ({
  taskStore: {
    get list() {
      return mockTaskList
    },
  },
}))

vi.mock('../components/TaskCard.svelte', () => ({ default: () => {} }))
vi.mock('../components/WorktreeList.svelte', () => ({ default: () => {} }))

vi.mock('@skeletonlabs/skeleton-svelte', () => ({
  SegmentedControl: Object.assign(() => {}, {
    Control: () => {},
    Indicator: () => {},
    Item: Object.assign(() => {}, {
      ItemText: () => {},
      ItemHiddenInput: () => {},
    }),
  }),
}))

beforeAll(() => {
  globalThis.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  } as unknown as typeof ResizeObserver
})

const ProjectDetail = (await import('./ProjectDetail.svelte')).default

const mockProject = {
  id: 'owner/repo',
  owner: 'owner',
  repo: 'repo',
  name: 'owner/repo',
  url: 'https://github.com/owner/repo',
  type: 'pet',
  clonePath: '/path/to/clone',
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
}

describe('ProjectDetail', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mockTaskList.length = 0
  })

  afterEach(() => {
    cleanup()
  })

  it('shows back to projects button', () => {
    mockGet.mockReturnValue(new Promise(() => {}))
    render(ProjectDetail, {
      props: { projectId: 'owner/repo', onback: vi.fn(), onviewtask: vi.fn() },
    })
    expect(screen.getByText('Back to projects')).toBeDefined()
  })

  it('shows loading state before project loads', () => {
    mockGet.mockReturnValue(new Promise(() => {}))
    render(ProjectDetail, {
      props: { projectId: 'owner/repo', onback: vi.fn(), onviewtask: vi.fn() },
    })
    expect(screen.getByText('Loading...')).toBeDefined()
  })

  it('shows project name after loading', async () => {
    mockGet.mockResolvedValue(mockProject)
    render(ProjectDetail, {
      props: { projectId: 'owner/repo', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText('owner/repo')).toBeDefined()
    })
  })

  it('shows error when project load fails', async () => {
    mockGet.mockRejectedValue(new Error('not found'))
    render(ProjectDetail, {
      props: { projectId: 'owner/repo', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText('Error: not found')).toBeDefined()
    })
  })

  it('shows pet type badge', async () => {
    mockGet.mockResolvedValue({ ...mockProject, type: 'pet' })
    render(ProjectDetail, {
      props: { projectId: 'owner/repo', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText('pet')).toBeDefined()
    })
  })

  it('shows work type badge', async () => {
    mockGet.mockResolvedValue({ ...mockProject, type: 'work' })
    render(ProjectDetail, {
      props: { projectId: 'owner/repo', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText('work')).toBeDefined()
    })
  })

  it('shows no tasks message when project has no tasks', async () => {
    mockGet.mockResolvedValue(mockProject)
    render(ProjectDetail, {
      props: { projectId: 'owner/repo', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText('No tasks assigned to this project')).toBeDefined()
    })
  })

  it('shows Delete button after loading', async () => {
    mockGet.mockResolvedValue(mockProject)
    render(ProjectDetail, {
      props: { projectId: 'owner/repo', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText('Delete')).toBeDefined()
    })
  })

  it('calls onback after successful delete', async () => {
    mockGet.mockResolvedValue(mockProject)
    mockRemove.mockResolvedValue(undefined)
    const onback = vi.fn()
    render(ProjectDetail, {
      props: { projectId: 'owner/repo', onback, onviewtask: vi.fn() },
    })
    await vi.waitFor(() => screen.getByText('Delete'))
    screen.getByText('Delete').click()
    await vi.waitFor(() => {
      expect(mockRemove).toHaveBeenCalledWith('owner/repo')
      expect(onback).toHaveBeenCalled()
    })
  })
})
