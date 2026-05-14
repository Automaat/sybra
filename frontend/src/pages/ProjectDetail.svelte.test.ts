import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

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

  it('renders project URL as external link', async () => {
    mockGet.mockResolvedValue(mockProject)
    render(ProjectDetail, {
      props: { projectId: 'owner/repo', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      const link = screen.getByText('https://github.com/owner/repo') as HTMLAnchorElement
      expect(link).toBeDefined()
      expect(link.getAttribute('href')).toBe('https://github.com/owner/repo')
      expect(link.getAttribute('target')).toBe('_blank')
      expect(link.getAttribute('rel')).toBe('noopener')
    })
  })

  it('renders clone path', async () => {
    mockGet.mockResolvedValue(mockProject)
    render(ProjectDetail, {
      props: { projectId: 'owner/repo', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText('/path/to/clone')).toBeDefined()
    })
  })

  it('renders Created and Updated timestamps', async () => {
    mockGet.mockResolvedValue(mockProject)
    render(ProjectDetail, {
      props: { projectId: 'owner/repo', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText(/Created:/)).toBeDefined()
      expect(screen.getByText(/Updated:/)).toBeDefined()
    })
  })

  it('shows Switch to Work button when type is pet', async () => {
    mockGet.mockResolvedValue({ ...mockProject, type: 'pet' })
    render(ProjectDetail, {
      props: { projectId: 'owner/repo', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText('Switch to Work')).toBeDefined()
    })
  })

  it('shows Switch to Pet button when type is work', async () => {
    mockGet.mockResolvedValue({ ...mockProject, type: 'work' })
    render(ProjectDetail, {
      props: { projectId: 'owner/repo', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText('Switch to Pet')).toBeDefined()
    })
  })

  it('calls projectStore.update with toggled type when toggle clicked', async () => {
    mockGet.mockResolvedValue({ ...mockProject, type: 'pet' })
    mockUpdate.mockResolvedValue({ ...mockProject, type: 'work' })
    render(ProjectDetail, {
      props: { projectId: 'owner/repo', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => screen.getByText('Switch to Work'))
    await fireEvent.click(screen.getByText('Switch to Work'))
    await vi.waitFor(() => {
      expect(mockUpdate).toHaveBeenCalledWith('owner/repo', 'work')
    })
  })

  it('toggles from work to pet', async () => {
    mockGet.mockResolvedValue({ ...mockProject, type: 'work' })
    mockUpdate.mockResolvedValue({ ...mockProject, type: 'pet' })
    render(ProjectDetail, {
      props: { projectId: 'owner/repo', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => screen.getByText('Switch to Pet'))
    await fireEvent.click(screen.getByText('Switch to Pet'))
    await vi.waitFor(() => {
      expect(mockUpdate).toHaveBeenCalledWith('owner/repo', 'pet')
    })
  })

  it('surfaces error when type toggle fails', async () => {
    mockGet.mockResolvedValue({ ...mockProject, type: 'pet' })
    mockUpdate.mockRejectedValue(new Error('update failed'))
    render(ProjectDetail, {
      props: { projectId: 'owner/repo', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => screen.getByText('Switch to Work'))
    await fireEvent.click(screen.getByText('Switch to Work'))
    await vi.waitFor(() => {
      expect(screen.getByText(/update failed/)).toBeDefined()
    })
  })

  it('surfaces error when delete fails and resets deleting flag', async () => {
    mockGet.mockResolvedValue(mockProject)
    mockRemove.mockRejectedValue(new Error('delete failed'))
    render(ProjectDetail, {
      props: { projectId: 'owner/repo', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => screen.getByText('Delete'))
    await fireEvent.click(screen.getByText('Delete'))
    await vi.waitFor(() => {
      expect(screen.getByText(/delete failed/)).toBeDefined()
      expect(screen.getByText('Delete')).toBeDefined()
    })
  })

  it('shows task count when project has tasks', async () => {
    mockGet.mockResolvedValue(mockProject)
    mockTaskList.push(
      { id: 't1', projectId: 'owner/repo', status: 'todo', title: 'Task 1' },
      { id: 't2', projectId: 'owner/repo', status: 'in-progress', title: 'Task 2' },
      { id: 't3', projectId: 'other/repo', status: 'todo', title: 'Other' },
    )
    render(ProjectDetail, {
      props: { projectId: 'owner/repo', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText('Tasks (2)')).toBeDefined()
    })
  })

  it('renders board columns when project has tasks', async () => {
    mockGet.mockResolvedValue(mockProject)
    mockTaskList.push({ id: 't1', projectId: 'owner/repo', status: 'todo', title: 'Task 1' })
    render(ProjectDetail, {
      props: { projectId: 'owner/repo', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText('Todo')).toBeDefined()
      expect(screen.getByText('In Progress')).toBeDefined()
      expect(screen.getByText('In Review')).toBeDefined()
      expect(screen.getByText('Testing')).toBeDefined()
      expect(screen.getByText('Human Required')).toBeDefined()
    })
  })

  it('filters out tasks not assigned to current project', async () => {
    mockGet.mockResolvedValue(mockProject)
    mockTaskList.push(
      { id: 't1', projectId: 'other/repo', status: 'todo', title: 'Other' },
    )
    render(ProjectDetail, {
      props: { projectId: 'owner/repo', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText('No tasks assigned to this project')).toBeDefined()
    })
  })

  it('disables Delete button while deleting', async () => {
    mockGet.mockResolvedValue(mockProject)
    let resolveRemove: () => void = () => {}
    mockRemove.mockReturnValue(new Promise<void>((res) => { resolveRemove = res }))
    render(ProjectDetail, {
      props: { projectId: 'owner/repo', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => screen.getByText('Delete'))
    await fireEvent.click(screen.getByText('Delete'))
    await vi.waitFor(() => {
      expect(screen.getByText('Deleting...')).toBeDefined()
    })
    resolveRemove()
  })
})
