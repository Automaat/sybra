import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const mockOpenLink = vi.fn()
let mockList: unknown[] = []

vi.mock('../../stores/tasks.svelte.js', () => ({
  taskStore: {
    get list() {
      return mockList
    },
  },
}))

vi.mock('$lib/browser.svelte.js', () => ({
  openLink: (...args: unknown[]) => mockOpenLink(...args),
}))

const TaskChildrenPanel = (await import('./TaskChildrenPanel.svelte')).default

const umbrella = {
  id: 'u1',
  title: 'Umbrella',
  status: 'todo',
  agentMode: 'headless',
  tags: [],
  taskType: 'umbrella',
  issue: 'https://github.com/Automaat/sybra/issues/1213',
  dependsOn: [
    'https://github.com/Automaat/sybra/issues/10',
    'https://github.com/Automaat/sybra/issues/99',
  ],
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
}

function child(overrides: Record<string, unknown>) {
  return {
    id: 'c1',
    title: 'Child',
    status: 'todo',
    agentMode: 'headless',
    tags: [],
    issue: 'https://github.com/Automaat/sybra/issues/10',
    umbrellaIssue: 'https://github.com/Automaat/sybra/issues/1213',
    createdAt: '2026-04-01T00:00:00Z',
    updatedAt: '2026-04-01T00:00:00Z',
    ...overrides,
  }
}

describe('TaskChildrenPanel', () => {
  beforeEach(() => {
    mockOpenLink.mockReset()
    mockList = []
  })
  afterEach(cleanup)

  it('shows a placeholder when there are no rows', () => {
    const solo = { ...umbrella, taskType: 'normal', dependsOn: [] }
    render(TaskChildrenPanel, { props: { task: solo as never, onselecttask: vi.fn() } })
    expect(screen.getByText('No child tasks linked yet.')).toBeDefined()
  })

  it('renders materialized child rows and unresolved refs', () => {
    mockList = [child({ id: 'c10', title: 'Ten', issue: 'https://github.com/Automaat/sybra/issues/10' })]
    render(TaskChildrenPanel, { props: { task: umbrella as never, onselecttask: vi.fn() } })
    expect(screen.getByText('Ten')).toBeDefined()
    expect(screen.getByText('automaat/sybra#99')).toBeDefined()
    expect(screen.getByText('Not yet tracked')).toBeDefined()
  })

  it('renders a tracker-declared local child that lacks umbrellaIssue', () => {
    mockList = [
      child({
        id: 'c10',
        title: 'Declared local child',
        issue: 'https://github.com/Automaat/sybra/issues/10',
        umbrellaIssue: '',
      }),
    ]
    render(TaskChildrenPanel, { props: { task: umbrella as never, onselecttask: vi.fn() } })
    expect(screen.getByText('Declared local child')).toBeDefined()
    expect(screen.getByText('automaat/sybra#99')).toBeDefined()
  })

  it('does not count a local-done child with no merged outcome as shipped', () => {
    mockList = [child({ id: 'c10', title: 'Ten', status: 'done', outcome: '' })]
    render(TaskChildrenPanel, { props: { task: umbrella as never, onselecttask: vi.fn() } })
    expect(screen.getByText('0/1 local merged-outcome progress')).toBeDefined()
  })

  it('counts a merged-outcome child as shipped regardless of local status', () => {
    mockList = [child({ id: 'c10', title: 'Ten', status: 'todo', outcome: 'merged' })]
    render(TaskChildrenPanel, { props: { task: umbrella as never, onselecttask: vi.fn() } })
    expect(screen.getByText('1/1 local merged-outcome progress')).toBeDefined()
  })

  it('navigates to the child task when its row is clicked', async () => {
    mockList = [child({ id: 'c10', title: 'Ten' })]
    const onselecttask = vi.fn()
    render(TaskChildrenPanel, { props: { task: umbrella as never, onselecttask } })
    await fireEvent.click(screen.getByText('Ten'))
    expect(onselecttask).toHaveBeenCalledWith('c10')
  })

  it('opens the GitHub issue link without triggering row navigation', async () => {
    mockList = [child({ id: 'c10', title: 'Ten' })]
    const onselecttask = vi.fn()
    render(TaskChildrenPanel, { props: { task: umbrella as never, onselecttask } })
    await fireEvent.click(screen.getByRole('button', { name: 'Open issue on GitHub' }))
    expect(mockOpenLink).toHaveBeenCalledWith('https://github.com/Automaat/sybra/issues/10', expect.anything())
    expect(onselecttask).not.toHaveBeenCalled()
  })

  it('opens the linked PR without triggering row navigation', async () => {
    mockList = [child({ id: 'c10', title: 'Ten', prNumber: 42, projectId: 'Automaat/sybra' })]
    const onselecttask = vi.fn()
    render(TaskChildrenPanel, { props: { task: umbrella as never, onselecttask } })
    await fireEvent.click(screen.getByRole('button', { name: 'Open PR #42 on GitHub' }))
    expect(mockOpenLink).toHaveBeenCalledWith('https://github.com/Automaat/sybra/pull/42', expect.anything())
    expect(onselecttask).not.toHaveBeenCalled()
  })

  it('opens an unresolved ref link without crashing', async () => {
    render(TaskChildrenPanel, { props: { task: umbrella as never, onselecttask: vi.fn() } })
    await fireEvent.click(screen.getByText('automaat/sybra#99'))
    expect(mockOpenLink).toHaveBeenCalledWith('https://github.com/automaat/sybra/issues/99', expect.anything())
  })
})
