import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, waitFor, cleanup } from '@testing-library/svelte'

const mockReassign = vi.fn()

vi.mock('../../stores/tasks.svelte.js', () => ({
  taskStore: { reassign: (...args: unknown[]) => mockReassign(...args) },
}))

vi.mock('../../stores/cluster.svelte.js', () => ({
  clusterStore: {
    get enabled() {
      return clusterEnabled
    },
    get names() {
      return clusterNames
    },
    statusOf: (n: string | undefined) => (n === 'pet-box' ? 'online' : ''),
  },
}))

let clusterEnabled = true
let clusterNames: string[] = ['pet-box', 'gpu-box']

const TaskNodeAssignment = (await import('./TaskNodeAssignment.svelte')).default

function task(overrides: Record<string, unknown> = {}) {
  return { id: 't1', title: 'x', status: 'in-progress', ...overrides } as never
}

describe('TaskNodeAssignment', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    clusterEnabled = true
    clusterNames = ['pet-box', 'gpu-box']
    mockReassign.mockResolvedValue(undefined)
  })

  afterEach(cleanup)

  it('renders nothing when the instance is not a cluster leader', () => {
    clusterEnabled = false
    render(TaskNodeAssignment, { props: { task: task() } })
    expect(screen.queryByTestId('task-node-assignment')).toBeNull()
  })

  it('shows local for a task with no assigned node', () => {
    render(TaskNodeAssignment, { props: { task: task() } })
    const select = screen.getByLabelText('Execution node') as HTMLSelectElement
    expect(select.value).toBe('local')
  })

  it('offers local plus every roster node', () => {
    render(TaskNodeAssignment, { props: { task: task({ assignedNode: 'pet-box' }) } })
    const options = screen.getAllByRole('option').map((o) => (o as HTMLOptionElement).value)
    expect(options).toEqual(['local', 'pet-box', 'gpu-box'])
  })

  it('reassigns to the node the operator picks', async () => {
    render(TaskNodeAssignment, { props: { task: task({ assignedNode: 'pet-box' }) } })
    const select = screen.getByLabelText('Execution node')
    await fireEvent.change(select, { target: { value: 'gpu-box' } })
    await waitFor(() => expect(mockReassign).toHaveBeenCalledWith('t1', 'gpu-box'))
  })

  it('brings a task home when local is picked', async () => {
    render(TaskNodeAssignment, { props: { task: task({ assignedNode: 'pet-box' }) } })
    await fireEvent.change(screen.getByLabelText('Execution node'), { target: { value: 'local' } })
    await waitFor(() => expect(mockReassign).toHaveBeenCalledWith('t1', 'local'))
  })

  it('does not reassign a task to the node it already runs on', async () => {
    render(TaskNodeAssignment, { props: { task: task({ assignedNode: 'pet-box' }) } })
    await fireEvent.change(screen.getByLabelText('Execution node'), { target: { value: 'pet-box' } })
    expect(mockReassign).not.toHaveBeenCalled()
  })

  it('surfaces a refusal from the backend instead of failing silently', async () => {
    mockReassign.mockRejectedValue(new Error('work task withheld from untrusted node: "gpu-box"'))
    render(TaskNodeAssignment, { props: { task: task({ assignedNode: 'pet-box' }) } })
    await fireEvent.change(screen.getByLabelText('Execution node'), { target: { value: 'gpu-box' } })
    await waitFor(() => {
      expect(screen.getByTestId('task-node-error').textContent).toContain('withheld from untrusted node')
    })
  })
})
