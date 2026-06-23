import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, cleanup } from '@testing-library/svelte'

const mockProjectList: any[] = []
const mockProjectStore = {
  loading: false,
  error: '',
  get list() {
    return mockProjectList
  },
}

vi.mock('../stores/projects.svelte.js', () => ({
  projectStore: mockProjectStore,
}))

const mockTaskList: any[] = []
vi.mock('../stores/tasks.svelte.js', () => ({
  taskStore: { get list() { return mockTaskList } },
}))

let mockPRsByRepo: Record<string, any[]> = {}
vi.mock('../stores/reviews.svelte.js', () => ({
  reviewStore: { byRepo: (repo: string) => mockPRsByRepo[repo] ?? [] },
}))

const ProjectList = (await import('./ProjectList.svelte')).default

const mockProject = {
  id: 'owner/repo',
  owner: 'owner',
  repo: 'repo',
  name: 'owner/repo',
  type: 'pet',
  url: 'https://github.com/owner/repo',
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
}

describe('ProjectList', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mockProjectList.length = 0
    mockTaskList.length = 0
    mockPRsByRepo = {}
    mockProjectStore.loading = false
    mockProjectStore.error = ''
  })

  afterEach(() => {
    cleanup()
  })

  it('does not render an in-body header or duplicate create button', () => {
    mockProjectList.push(mockProject)
    render(ProjectList, { props: { onselect: vi.fn(), onadd: vi.fn() } })
    // The page title + create button live in the app top bar now.
    expect(screen.queryByText('Projects')).toBeNull()
    expect(screen.queryByText('+ Add Project')).toBeNull()
  })

  it('calls onadd from the empty-state button', async () => {
    const onadd = vi.fn()
    render(ProjectList, { props: { onselect: vi.fn(), onadd } })
    await fireEvent.click(screen.getByText('Add your first project'))
    expect(onadd).toHaveBeenCalledOnce()
  })

  it('shows loading state', () => {
    mockProjectStore.loading = true
    render(ProjectList, { props: { onselect: vi.fn(), onadd: vi.fn() } })
    expect(screen.getByText('Loading projects...')).toBeDefined()
  })

  it('shows error state', () => {
    mockProjectStore.error = 'Connection failed'
    render(ProjectList, { props: { onselect: vi.fn(), onadd: vi.fn() } })
    expect(screen.getByText('Connection failed')).toBeDefined()
  })

  it('shows empty state when no projects', () => {
    render(ProjectList, { props: { onselect: vi.fn(), onadd: vi.fn() } })
    expect(screen.getByText('No projects yet')).toBeDefined()
  })

  it('shows Add your first project button in empty state', () => {
    render(ProjectList, { props: { onselect: vi.fn(), onadd: vi.fn() } })
    expect(screen.getByText('Add your first project')).toBeDefined()
  })

  it('renders project owner/repo', () => {
    mockProjectList.push(mockProject)
    render(ProjectList, { props: { onselect: vi.fn(), onadd: vi.fn() } })
    expect(screen.getByText('owner/repo')).toBeDefined()
  })

  it('shows pet type badge', () => {
    mockProjectList.push({ ...mockProject, type: 'pet' })
    render(ProjectList, { props: { onselect: vi.fn(), onadd: vi.fn() } })
    expect(screen.getByText('pet')).toBeDefined()
  })

  it('shows work type badge', () => {
    mockProjectList.push({ ...mockProject, type: 'work' })
    render(ProjectList, { props: { onselect: vi.fn(), onadd: vi.fn() } })
    expect(screen.getByText('work')).toBeDefined()
  })

  it('calls onselect when project clicked', async () => {
    mockProjectList.push(mockProject)
    const onselect = vi.fn()
    render(ProjectList, { props: { onselect, onadd: vi.fn() } })
    await fireEvent.click(screen.getByText('owner/repo'))
    expect(onselect).toHaveBeenCalledWith('owner/repo')
  })

  it('shows active-task and open-PR counts on the card', () => {
    mockProjectList.push(mockProject)
    mockTaskList.push(
      { projectId: 'owner/repo', status: 'in-progress', updatedAt: '2026-04-02T00:00:00Z' },
      { projectId: 'owner/repo', status: 'done', updatedAt: '2026-04-01T00:00:00Z' },
      { projectId: 'owner/repo', status: 'mystery', updatedAt: '2026-04-01T00:00:00Z' },
      { projectId: 'other/repo', status: 'todo', updatedAt: '2026-04-03T00:00:00Z' },
    )
    mockPRsByRepo = { 'owner/repo': [{ number: 1 }, { number: 2 }] }
    render(ProjectList, { props: { onselect: vi.fn(), onadd: vi.fn() } })
    // done, unknown status, and the other project are all excluded from "active".
    expect(screen.getByText('1 active')).toBeDefined()
    expect(screen.getByText('2 PRs')).toBeDefined()
  })
})
