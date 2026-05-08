import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const agentOutputsMap = new Map<string, any[]>()

vi.mock('../stores/agents.svelte.js', () => ({
  agentStore: {
    get outputs() { return agentOutputsMap },
  },
}))

const AgentErrorBanner = (await import('./AgentErrorBanner.svelte')).default

describe('AgentErrorBanner', () => {
  afterEach(() => {
    cleanup()
    agentOutputsMap.clear()
  })

  it('shows worktree_conflict label', () => {
    render(AgentErrorBanner, {
      props: { agentId: 'a1', error: { kind: 'worktree_conflict', msg: '' } },
    })
    expect(screen.getByText('Worktree conflict')).toBeDefined()
  })

  it('shows kind badge', () => {
    render(AgentErrorBanner, {
      props: { agentId: 'a1', error: { kind: 'worktree_conflict', msg: '' } },
    })
    expect(screen.getByText('worktree_conflict')).toBeDefined()
  })

  it('shows git_clone error label', () => {
    render(AgentErrorBanner, {
      props: { agentId: 'a1', error: { kind: 'git_clone', msg: '' } },
    })
    expect(screen.getByText('Git / network error')).toBeDefined()
  })

  it('shows rate_limit error label', () => {
    render(AgentErrorBanner, {
      props: { agentId: 'a1', error: { kind: 'rate_limit', msg: '' } },
    })
    expect(screen.getByText('API rate limited')).toBeDefined()
  })

  it('shows crash label for unknown kind', () => {
    render(AgentErrorBanner, {
      props: { agentId: 'a1', error: { kind: 'unknown_kind', msg: '' } },
    })
    expect(screen.getByText('Agent crashed')).toBeDefined()
  })

  it('shows permission_denied label', () => {
    render(AgentErrorBanner, {
      props: { agentId: 'a1', error: { kind: 'permission_denied', msg: '' } },
    })
    expect(screen.getByText('Permission denied')).toBeDefined()
  })

  it('shows Retry button when onretry provided', () => {
    render(AgentErrorBanner, {
      props: { agentId: 'a1', error: { kind: 'crash', msg: '' }, onretry: vi.fn() },
    })
    expect(screen.getByText('Retry')).toBeDefined()
  })

  it('calls onretry when Retry clicked', async () => {
    const onretry = vi.fn()
    render(AgentErrorBanner, {
      props: { agentId: 'a1', error: { kind: 'crash', msg: '' }, onretry },
    })
    await fireEvent.click(screen.getByText('Retry'))
    expect(onretry).toHaveBeenCalled()
  })

  it('does not show Retry button when onretry not provided', () => {
    render(AgentErrorBanner, {
      props: { agentId: 'a1', error: { kind: 'crash', msg: '' } },
    })
    expect(screen.queryByText('Retry')).toBeNull()
  })

  it('shows Dismiss button when ondismiss provided', () => {
    render(AgentErrorBanner, {
      props: { agentId: 'a1', error: { kind: 'crash', msg: '' }, ondismiss: vi.fn() },
    })
    expect(screen.getByText('Dismiss')).toBeDefined()
  })

  it('calls ondismiss when Dismiss clicked', async () => {
    const ondismiss = vi.fn()
    render(AgentErrorBanner, {
      props: { agentId: 'a1', error: { kind: 'crash', msg: '' }, ondismiss },
    })
    await fireEvent.click(screen.getByText('Dismiss'))
    expect(ondismiss).toHaveBeenCalled()
  })

  it('shows extra msg when different from spec.what', () => {
    render(AgentErrorBanner, {
      props: {
        agentId: 'a1',
        error: { kind: 'crash', msg: 'exit code 137' },
      },
    })
    expect(screen.getByText('exit code 137')).toBeDefined()
  })

  it('shows View logs button when agent has outputs', () => {
    agentOutputsMap.set('a1', [{ event: { content: 'log line 1' } }])
    render(AgentErrorBanner, {
      props: { agentId: 'a1', error: { kind: 'crash', msg: '' } },
    })
    expect(screen.getByText('View logs')).toBeDefined()
  })

  it('toggles log panel on View logs click', async () => {
    agentOutputsMap.set('a1', [{ event: { content: 'error detail' } }])
    render(AgentErrorBanner, {
      props: { agentId: 'a1', error: { kind: 'crash', msg: '' } },
    })
    await fireEvent.click(screen.getByText('View logs'))
    expect(screen.getByText('error detail')).toBeDefined()
  })

  it('does not show View logs when no outputs', () => {
    render(AgentErrorBanner, {
      props: { agentId: 'a1', error: { kind: 'crash', msg: '' } },
    })
    expect(screen.queryByText('View logs')).toBeNull()
  })
})
