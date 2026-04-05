import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'

const mockListWorktrees = vi.fn()
const mockOpenInTerminal = vi.fn()
const mockOpenInEditor = vi.fn()

vi.mock('../../wailsjs/go/main/App.js', () => ({
  ListWorktrees: (...args: unknown[]) => mockListWorktrees(...args),
  OpenInTerminal: (...args: unknown[]) => mockOpenInTerminal(...args),
  OpenInEditor: (...args: unknown[]) => mockOpenInEditor(...args),
}))

const WorktreeList = (await import('./WorktreeList.svelte')).default

const mockWorktree = {
  path: '/home/user/worktrees/task-1',
  branch: 'feat/my-feature',
  head: 'abc1234',
  taskId: 'task-1',
}

describe('WorktreeList', () => {
  beforeEach(() => {
    vi.clearAllMocks()
  })

  afterEach(() => {
    cleanup()
  })

  it('shows loading state initially', () => {
    mockListWorktrees.mockReturnValue(new Promise(() => {}))
    render(WorktreeList, { props: { projectId: 'owner/repo' } })
    expect(screen.getByText('Loading worktrees...')).toBeDefined()
  })

  it('shows empty state when no worktrees', async () => {
    mockListWorktrees.mockResolvedValue([])
    render(WorktreeList, { props: { projectId: 'owner/repo' } })
    await vi.waitFor(() => {
      expect(screen.getByText('No active worktrees')).toBeDefined()
    })
  })

  it('shows error when load fails', async () => {
    mockListWorktrees.mockRejectedValue(new Error('git error'))
    render(WorktreeList, { props: { projectId: 'owner/repo' } })
    await vi.waitFor(() => {
      expect(screen.getByText('Error: git error')).toBeDefined()
    })
  })

  it('shows worktree branch', async () => {
    mockListWorktrees.mockResolvedValue([mockWorktree])
    render(WorktreeList, { props: { projectId: 'owner/repo' } })
    await vi.waitFor(() => {
      expect(screen.getByText('feat/my-feature')).toBeDefined()
    })
  })

  it('shows worktree commit hash', async () => {
    mockListWorktrees.mockResolvedValue([mockWorktree])
    render(WorktreeList, { props: { projectId: 'owner/repo' } })
    await vi.waitFor(() => {
      expect(screen.getByText('abc1234')).toBeDefined()
    })
  })

  it('shows worktree path', async () => {
    mockListWorktrees.mockResolvedValue([mockWorktree])
    render(WorktreeList, { props: { projectId: 'owner/repo' } })
    await vi.waitFor(() => {
      expect(screen.getByText('/home/user/worktrees/task-1')).toBeDefined()
    })
  })

  it('shows task id when present', async () => {
    mockListWorktrees.mockResolvedValue([mockWorktree])
    render(WorktreeList, { props: { projectId: 'owner/repo' } })
    await vi.waitFor(() => {
      expect(screen.getByText('Task: task-1')).toBeDefined()
    })
  })

  it('does not show task id when absent', async () => {
    mockListWorktrees.mockResolvedValue([{ ...mockWorktree, taskId: '' }])
    render(WorktreeList, { props: { projectId: 'owner/repo' } })
    await vi.waitFor(() => {
      expect(screen.queryByText(/^Task:/)).toBeNull()
    })
  })

  it('shows terminal and editor buttons', async () => {
    mockListWorktrees.mockResolvedValue([mockWorktree])
    render(WorktreeList, { props: { projectId: 'owner/repo' } })
    await vi.waitFor(() => {
      expect(screen.getByTitle('Open in Ghostty')).toBeDefined()
      expect(screen.getByTitle('Open in Zed')).toBeDefined()
    })
  })
})
