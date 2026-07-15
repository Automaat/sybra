import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'
import { Definition, Trigger } from '../../bindings/github.com/Automaat/sybra/internal/workflow/models.js'

const mockWorkflowGet = vi.fn()
const mockWorkflowSave = vi.fn()

vi.mock('../stores/workflows.svelte.js', () => ({
  workflowStore: {
    get: (...args: unknown[]) => mockWorkflowGet(...args),
    save: (...args: unknown[]) => mockWorkflowSave(...args),
  },
}))

vi.mock('../components/workflow/WorkflowGraph.svelte', () => ({ default: () => {} }))
vi.mock('../components/workflow/StepConfigPanel.svelte', () => ({ default: () => {} }))
vi.mock('../components/workflow/TriggerConfigPanel.svelte', () => ({ default: () => {} }))
vi.mock('../components/MobileWorkflowNotice.svelte', () => ({ default: () => {} }))

vi.mock('../lib/viewport.svelte.js', () => ({
  viewport: { isDesktop: true, isMobile: false },
}))

vi.mock('../lib/workflow-graph.js', () => ({
  definitionToGraph: vi.fn(() => ({ nodes: [], edges: [] })),
  graphToDefinition: vi.fn((def: unknown) => def),
  TRIGGER_NODE_ID: 'trigger',
}))

const WorkflowDetail = (await import('./WorkflowDetail.svelte')).default

function makeDef(overrides: Record<string, unknown> = {}) {
  return Definition.createFrom({
    id: 'wf-1',
    name: 'my-workflow',
    description: 'Does things',
    trigger: Trigger.createFrom({ on: 'manual', conditions: [] }),
    steps: [],
    builtin: false,
    ...overrides,
  })
}

describe('WorkflowDetail', () => {
  beforeEach(() => {
    mockWorkflowGet.mockReset()
    mockWorkflowSave.mockReset()
  })

  afterEach(() => {
    cleanup()
    vi.restoreAllMocks()
  })

  it('shows "Loading .." while definition loads', () => {
    mockWorkflowGet.mockReturnValue(new Promise(() => {}))
    render(WorkflowDetail, { props: { workflowId: 'wf-1', onback: vi.fn() } })
    expect(screen.getByText('Loading workflow...')).toBeDefined()
  })

  it('renders workflow name after load', async () => {
    mockWorkflowGet.mockResolvedValue(makeDef({ name: 'deploy-pipeline' }))
    render(WorkflowDetail, { props: { workflowId: 'wf-1', onback: vi.fn() } })
    await vi.waitFor(() => {
      expect(screen.getByText('deploy-pipeline')).toBeDefined()
    })
  })

  it('calls onback when Back button clicked', async () => {
    mockWorkflowGet.mockResolvedValue(makeDef())
    const onback = vi.fn()
    render(WorkflowDetail, { props: { workflowId: 'wf-1', onback } })
    await vi.waitFor(() => {
      expect(screen.getByText('my-workflow')).toBeDefined()
    })
    const backBtn = screen.getByRole('button', { name: /Back/ })
    await fireEvent.click(backBtn)
    expect(onback).toHaveBeenCalled()
  })

  it('Save button is disabled when not dirty', async () => {
    mockWorkflowGet.mockResolvedValue(makeDef())
    render(WorkflowDetail, { props: { workflowId: 'wf-1', onback: vi.fn() } })
    await vi.waitFor(() => {
      expect(screen.getByText('my-workflow')).toBeDefined()
    })
    const saveBtn = screen.getByText('Save') as HTMLButtonElement
    expect(saveBtn.disabled).toBe(true)
  })

  it('Add step button is disabled while loading', () => {
    mockWorkflowGet.mockReturnValue(new Promise(() => {}))
    render(WorkflowDetail, { props: { workflowId: 'wf-1', onback: vi.fn() } })
    const addBtn = screen.getByText('+ Add step') as HTMLButtonElement
    expect(addBtn.disabled).toBe(true)
  })

  it('shows "built-in" badge for builtin workflows', async () => {
    mockWorkflowGet.mockResolvedValue(makeDef({ builtin: true }))
    render(WorkflowDetail, { props: { workflowId: 'wf-1', onback: vi.fn() } })
    await vi.waitFor(() => {
      expect(screen.getByText('built-in')).toBeDefined()
    })
  })

  it('does not show "built-in" badge for non-builtin workflows', async () => {
    mockWorkflowGet.mockResolvedValue(makeDef({ builtin: false }))
    render(WorkflowDetail, { props: { workflowId: 'wf-1', onback: vi.fn() } })
    await vi.waitFor(() => {
      expect(screen.getByText('my-workflow')).toBeDefined()
    })
    expect(screen.queryByText('built-in')).toBeNull()
  })

  it('shows "unsaved" indicator after adding a step', async () => {
    const { definitionToGraph } = await import('../lib/workflow-graph.js')
    vi.mocked(definitionToGraph).mockReturnValue({ nodes: [], edges: [] })
    mockWorkflowGet.mockResolvedValue(makeDef())
    render(WorkflowDetail, { props: { workflowId: 'wf-1', onback: vi.fn() } })
    await vi.waitFor(() => {
      expect(screen.getByText('my-workflow')).toBeDefined()
    })
    const addBtn = screen.getByText('+ Add step')
    await fireEvent.click(addBtn)
    await vi.waitFor(() => {
      expect(screen.getByText('unsaved')).toBeDefined()
    })
  })

  it('calls workflowStore.save when Save clicked after dirty', async () => {
    mockWorkflowGet.mockResolvedValue(makeDef())
    mockWorkflowSave.mockResolvedValue(makeDef())
    render(WorkflowDetail, { props: { workflowId: 'wf-1', onback: vi.fn() } })
    await vi.waitFor(() => {
      expect(screen.getByText('my-workflow')).toBeDefined()
    })
    // Make it dirty by adding a step
    await fireEvent.click(screen.getByText('+ Add step'))
    await vi.waitFor(() => {
      expect(screen.getByText('unsaved')).toBeDefined()
    })
    await fireEvent.click(screen.getByText('Save'))
    await vi.waitFor(() => {
      expect(mockWorkflowSave).toHaveBeenCalled()
    })
  })

  it('Escape key calls onback when nothing is selected', async () => {
    mockWorkflowGet.mockResolvedValue(makeDef())
    const onback = vi.fn()
    render(WorkflowDetail, { props: { workflowId: 'wf-1', onback } })
    await vi.waitFor(() => {
      expect(screen.getByText('my-workflow')).toBeDefined()
    })
    await fireEvent.keyDown(window, { key: 'Escape' })
    expect(onback).toHaveBeenCalled()
  })

  it('Cmd+S triggers save when dirty', async () => {
    mockWorkflowGet.mockResolvedValue(makeDef())
    mockWorkflowSave.mockResolvedValue(makeDef())
    render(WorkflowDetail, { props: { workflowId: 'wf-1', onback: vi.fn() } })
    await vi.waitFor(() => screen.getByText('my-workflow'))
    await fireEvent.click(screen.getByText('+ Add step'))
    await vi.waitFor(() => screen.getByText('unsaved'))
    await fireEvent.keyDown(window, { key: 's', metaKey: true })
    await vi.waitFor(() => {
      expect(mockWorkflowSave).toHaveBeenCalled()
    })
  })

  it('clears unsaved indicator after successful save', async () => {
    mockWorkflowGet.mockResolvedValue(makeDef())
    mockWorkflowSave.mockResolvedValue(makeDef())
    render(WorkflowDetail, { props: { workflowId: 'wf-1', onback: vi.fn() } })
    await vi.waitFor(() => screen.getByText('my-workflow'))
    await fireEvent.click(screen.getByText('+ Add step'))
    await vi.waitFor(() => screen.getByText('unsaved'))
    await fireEvent.click(screen.getByText('Save'))
    await vi.waitFor(() => {
      expect(screen.queryByText('unsaved')).toBeNull()
    })
  })

  it('+ Add step is enabled once definition loads', async () => {
    mockWorkflowGet.mockResolvedValue(makeDef())
    render(WorkflowDetail, { props: { workflowId: 'wf-1', onback: vi.fn() } })
    await vi.waitFor(() => screen.getByText('my-workflow'))
    const addBtn = screen.getByText('+ Add step') as HTMLButtonElement
    expect(addBtn.disabled).toBe(false)
  })
})
