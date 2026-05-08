import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'

vi.mock('$lib/bash-activity.js', () => ({
  extractBashActivity: vi.fn(() => []),
  stripAnsi: (s: string) => s,
  truncateOutput: (s: string) => s,
}))

const { extractBashActivity } = await import('$lib/bash-activity.js')
const ShellTab = (await import('./ShellTab.svelte')).default

function makeActivity(overrides: Record<string, unknown> = {}) {
  return {
    id: 'act-1',
    ts: new Date('2026-04-01T00:00:00Z'),
    command: 'ls -la',
    output: '',
    status: 'done' as const,
    isError: false,
    ...overrides,
  }
}

describe('ShellTab', () => {
  afterEach(() => {
    cleanup()
    vi.mocked(extractBashActivity).mockReturnValue([])
  })

  it('shows empty state when no activities', () => {
    render(ShellTab, { props: { streamOutputs: [], convoEvents: [] } })
    expect(screen.getByText('No shell activity yet')).toBeDefined()
  })

  it('shows command when activities exist', () => {
    vi.mocked(extractBashActivity).mockReturnValue([makeActivity({ command: 'npm test' })])
    render(ShellTab, { props: { streamOutputs: [], convoEvents: [] } })
    expect(screen.getByText('npm test')).toBeDefined()
  })

  it('shows "running…" label for running activities', () => {
    vi.mocked(extractBashActivity).mockReturnValue([makeActivity({ status: 'running' })])
    render(ShellTab, { props: { streamOutputs: [], convoEvents: [] } })
    expect(screen.getByText('running…')).toBeDefined()
  })

  it('shows output details section when output exists', () => {
    vi.mocked(extractBashActivity).mockReturnValue([makeActivity({ output: 'file1.ts\nfile2.ts' })])
    render(ShellTab, { props: { streamOutputs: [], convoEvents: [] } })
    expect(screen.getByText('Output')).toBeDefined()
  })

  it('renders multiple activities', () => {
    vi.mocked(extractBashActivity).mockReturnValue([
      makeActivity({ id: 'a1', command: 'cmd-one' }),
      makeActivity({ id: 'a2', command: 'cmd-two' }),
    ])
    render(ShellTab, { props: { streamOutputs: [], convoEvents: [] } })
    expect(screen.getByText('cmd-one')).toBeDefined()
    expect(screen.getByText('cmd-two')).toBeDefined()
  })
})

describe('PlannerTab', () => {
  afterEach(() => { cleanup() })

  it('shows empty state when no plan steps', async () => {
    vi.mock('../PlanSteps.svelte', () => ({ default: () => {} }))
    const PlannerTab = (await import('./PlannerTab.svelte')).default
    render(PlannerTab, { props: { planSteps: [] } })
    expect(screen.getByText('No plan yet')).toBeDefined()
  })
})
