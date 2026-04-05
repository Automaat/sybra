import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'

const mockListTmuxSessions = vi.fn()
const mockKillTmuxSession = vi.fn()
const mockAttachTmuxSession = vi.fn()

vi.mock('../../wailsjs/go/main/App.js', () => ({
  ListTmuxSessions: (...args: unknown[]) => mockListTmuxSessions(...args),
  KillTmuxSession: (...args: unknown[]) => mockKillTmuxSession(...args),
  AttachTmuxSession: (...args: unknown[]) => mockAttachTmuxSession(...args),
}))

const TmuxSessions = (await import('./TmuxSessions.svelte')).default

const mockSession = {
  name: 'synapse-task-1',
  created: '2026-04-01 10:00:00',
}

describe('TmuxSessions', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.useFakeTimers()
  })

  afterEach(() => {
    vi.useRealTimers()
    cleanup()
  })

  it('shows loading state initially', () => {
    mockListTmuxSessions.mockReturnValue(new Promise(() => {}))
    render(TmuxSessions, { props: {} })
    expect(screen.getByText('Loading sessions...')).toBeDefined()
  })

  it('shows empty state when no sessions', async () => {
    mockListTmuxSessions.mockResolvedValue([])
    render(TmuxSessions, { props: {} })
    await vi.waitFor(() => {
      expect(screen.getByText('No tmux sessions')).toBeDefined()
    })
  })

  it('shows error when load fails', async () => {
    mockListTmuxSessions.mockRejectedValue(new Error('tmux not found'))
    render(TmuxSessions, { props: {} })
    await vi.waitFor(() => {
      expect(screen.getByText('Error: tmux not found')).toBeDefined()
    })
  })

  it('shows session name', async () => {
    mockListTmuxSessions.mockResolvedValue([mockSession])
    render(TmuxSessions, { props: {} })
    await vi.waitFor(() => {
      expect(screen.getByText('synapse-task-1')).toBeDefined()
    })
  })

  it('shows session created date', async () => {
    mockListTmuxSessions.mockResolvedValue([mockSession])
    render(TmuxSessions, { props: {} })
    await vi.waitFor(() => {
      expect(screen.getByText('Created: 2026-04-01 10:00:00')).toBeDefined()
    })
  })

  it('shows Attach and Kill buttons', async () => {
    mockListTmuxSessions.mockResolvedValue([mockSession])
    render(TmuxSessions, { props: {} })
    await vi.waitFor(() => {
      expect(screen.getByText('Attach')).toBeDefined()
      expect(screen.getByText('Kill')).toBeDefined()
    })
  })

  it('shows session count', async () => {
    mockListTmuxSessions.mockResolvedValue([mockSession])
    render(TmuxSessions, { props: {} })
    await vi.waitFor(() => {
      expect(screen.getByText('1 session')).toBeDefined()
    })
  })

  it('shows plural sessions count', async () => {
    mockListTmuxSessions.mockResolvedValue([mockSession, { ...mockSession, name: 'synapse-task-2' }])
    render(TmuxSessions, { props: {} })
    await vi.waitFor(() => {
      expect(screen.getByText('2 sessions')).toBeDefined()
    })
  })

  it('shows Refresh button', async () => {
    mockListTmuxSessions.mockResolvedValue([])
    render(TmuxSessions, { props: {} })
    await vi.waitFor(() => {
      expect(screen.getByText('Refresh')).toBeDefined()
    })
  })
})
