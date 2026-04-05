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
    mockProjectStore.loading = false
    mockProjectStore.error = ''
  })

  afterEach(() => {
    cleanup()
  })

  it('renders Projects heading', () => {
    render(ProjectList, { props: { onselect: vi.fn(), onadd: vi.fn() } })
    expect(screen.getByText('Projects')).toBeDefined()
  })

  it('shows Add Project button', () => {
    render(ProjectList, { props: { onselect: vi.fn(), onadd: vi.fn() } })
    expect(screen.getByText('+ Add Project')).toBeDefined()
  })

  it('calls onadd when Add Project clicked', async () => {
    const onadd = vi.fn()
    render(ProjectList, { props: { onselect: vi.fn(), onadd } })
    await fireEvent.click(screen.getByText('+ Add Project'))
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
})
