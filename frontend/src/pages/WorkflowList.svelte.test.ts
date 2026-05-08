import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const workflowStoreMock = {
  list: [] as any[],
  loading: false,
  error: '',
  load: vi.fn(),
}

vi.mock('../stores/workflows.svelte.js', () => ({
  workflowStore: workflowStoreMock,
}))

const { workflowStore } = await import('../stores/workflows.svelte.js')
const WorkflowList = (await import('./WorkflowList.svelte')).default

function makeWorkflow(overrides: Record<string, unknown> = {}) {
  return {
    id: 'wf-1',
    name: 'my-workflow',
    description: 'Does things',
    builtin: false,
    steps: [],
    trigger: { on: 'manual', conditions: [] },
    ...overrides,
  }
}

describe('WorkflowList', () => {
  beforeEach(() => {
    Object.assign(workflowStore, { list: [], loading: false, error: '' })
    vi.mocked(workflowStore.load).mockClear()
  })

  afterEach(() => {
    cleanup()
    vi.restoreAllMocks()
  })

  it('renders Workflows heading', () => {
    render(WorkflowList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('Workflows')).toBeDefined()
  })

  it('shows loading message when loading with empty list', () => {
    Object.assign(workflowStore, { loading: true, list: [] })
    render(WorkflowList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('Loading workflows...')).toBeDefined()
  })

  it('shows error message when workflowStore has error', () => {
    Object.assign(workflowStore, { error: 'Failed to load', list: [] })
    render(WorkflowList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('Failed to load')).toBeDefined()
  })

  it('shows empty state when no workflows', () => {
    Object.assign(workflowStore, { list: [] })
    render(WorkflowList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('No workflows found')).toBeDefined()
  })

  it('renders workflow names', () => {
    Object.assign(workflowStore, {
      list: [makeWorkflow({ id: 'w1', name: 'deploy-pipeline' })],
    })
    render(WorkflowList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('deploy-pipeline')).toBeDefined()
  })

  it('renders workflow description', () => {
    Object.assign(workflowStore, {
      list: [makeWorkflow({ id: 'w1', description: 'Deploys to prod' })],
    })
    render(WorkflowList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('Deploys to prod')).toBeDefined()
  })

  it('shows "built-in" badge for builtin workflows', () => {
    Object.assign(workflowStore, {
      list: [makeWorkflow({ id: 'w1', builtin: true })],
    })
    render(WorkflowList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('built-in')).toBeDefined()
  })

  it('shows step count for each workflow', () => {
    Object.assign(workflowStore, {
      list: [makeWorkflow({ id: 'w1', steps: [{ id: 's1' }, { id: 's2' }] })],
    })
    render(WorkflowList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('2 steps')).toBeDefined()
  })

  it('shows trigger type in metadata', () => {
    Object.assign(workflowStore, {
      list: [makeWorkflow({ id: 'w1', trigger: { on: 'schedule', conditions: [] } })],
    })
    render(WorkflowList, { props: { onselect: vi.fn() } })
    expect(screen.getByText(/trigger: schedule/)).toBeDefined()
  })

  it('calls onselect with workflow id when card clicked', async () => {
    const onselect = vi.fn()
    Object.assign(workflowStore, {
      list: [makeWorkflow({ id: 'wf-42', name: 'click-me' })],
    })
    render(WorkflowList, { props: { onselect } })
    await fireEvent.click(screen.getByText('click-me'))
    expect(onselect).toHaveBeenCalledWith('wf-42')
  })

  it('shows condition count when trigger has conditions', () => {
    Object.assign(workflowStore, {
      list: [makeWorkflow({ id: 'w1', trigger: { on: 'task_updated', conditions: [{}] } })],
    })
    render(WorkflowList, { props: { onselect: vi.fn() } })
    expect(screen.getByText(/1 cond/)).toBeDefined()
  })
})
