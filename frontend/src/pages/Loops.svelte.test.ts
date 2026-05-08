import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const mockLoad = vi.fn()
const mockCreate = vi.fn()
const mockUpdate = vi.fn()
const mockRemove = vi.fn()
const mockRunNow = vi.fn()
const mockEventsOn = vi.fn((..._args: any[]) => vi.fn())

const loopStoreMock = {
  list: [] as any[],
  loading: false,
  error: '',
  load: (...args: unknown[]) => mockLoad(...args),
  create: (...args: unknown[]) => mockCreate(...args),
  update: (...args: unknown[]) => mockUpdate(...args),
  remove: (...args: unknown[]) => mockRemove(...args),
  runNow: (...args: unknown[]) => mockRunNow(...args),
}

vi.mock('../stores/loops.svelte.js', () => ({
  loopStore: loopStoreMock,
}))

vi.mock('../components/shell/MobileSheet.svelte', () => ({ default: () => {} }))

vi.mock('$lib/api', () => ({
  EventsOn: (...args: any[]) => mockEventsOn(...args),
}))

const { loopStore } = await import('../stores/loops.svelte.js')
const Loops = (await import('./Loops.svelte')).default

function makeLoop(overrides: Record<string, unknown> = {}) {
  return {
    id: 'loop-1',
    name: 'nightly-monitor',
    prompt: '/sybra-monitor',
    intervalSec: 3600,
    model: 'sonnet',
    enabled: true,
    lastRunAt: null,
    lastRunCost: 0,
    allowedTools: [],
    ...overrides,
  }
}

describe('Loops', () => {
  beforeEach(() => {
    mockLoad.mockReset()
    mockCreate.mockReset()
    mockUpdate.mockReset()
    mockRemove.mockReset()
    mockRunNow.mockReset()
    mockEventsOn.mockReturnValue(vi.fn())
    Object.assign(loopStore, { list: [], loading: false, error: '' })
  })

  afterEach(() => {
    cleanup()
    vi.restoreAllMocks()
  })

  it('shows "Loading loops..." when loading with empty list', () => {
    Object.assign(loopStore, { loading: true, list: [] })
    render(Loops)
    expect(screen.getByText('Loading loops...')).toBeDefined()
  })

  it('shows empty state when no loops exist', () => {
    Object.assign(loopStore, { loading: false, list: [] })
    render(Loops)
    expect(screen.getByText('No loop agents')).toBeDefined()
  })

  it('shows error message when loopStore has error', () => {
    Object.assign(loopStore, { error: 'Connection failed', list: [] })
    render(Loops)
    expect(screen.getByText('Connection failed')).toBeDefined()
  })

  it('shows loop count in header', () => {
    Object.assign(loopStore, { list: [makeLoop(), makeLoop({ id: 'loop-2', name: 'backup-monitor' })] })
    render(Loops)
    expect(screen.getByText('2 loops')).toBeDefined()
  })

  it('shows "1 loop" for single loop (no plural)', () => {
    Object.assign(loopStore, { list: [makeLoop()] })
    render(Loops)
    expect(screen.getByText('1 loop')).toBeDefined()
  })

  it('renders + New Loop button', () => {
    render(Loops)
    expect(screen.getByText('+ New Loop')).toBeDefined()
  })

  it('renders loop name and interval for each loop', () => {
    Object.assign(loopStore, { list: [makeLoop({ name: 'my-checker', intervalSec: 3600 })] })
    render(Loops)
    expect(screen.getByText('my-checker')).toBeDefined()
    expect(screen.getByText('every 1h')).toBeDefined()
  })

  it('shows "active" badge for enabled loops', () => {
    Object.assign(loopStore, { list: [makeLoop({ enabled: true })] })
    render(Loops)
    expect(screen.getByText('active')).toBeDefined()
  })

  it('shows "paused" badge for disabled loops', () => {
    Object.assign(loopStore, { list: [makeLoop({ enabled: false })] })
    render(Loops)
    expect(screen.getByText('paused')).toBeDefined()
  })

  it('renders Run now, Pause, Edit, Delete buttons for each loop', () => {
    Object.assign(loopStore, { list: [makeLoop({ enabled: true })] })
    render(Loops)
    expect(screen.getByText('Run now')).toBeDefined()
    expect(screen.getByText('Pause')).toBeDefined()
    expect(screen.getByText('Edit')).toBeDefined()
    expect(screen.getByText('Delete')).toBeDefined()
  })

  it('shows "Enable" button for disabled loops instead of Pause', () => {
    Object.assign(loopStore, { list: [makeLoop({ enabled: false })] })
    render(Loops)
    expect(screen.getByText('Enable')).toBeDefined()
    expect(screen.queryByText('Pause')).toBeNull()
  })

  it('calls loopStore.runNow when Run now clicked', async () => {
    mockRunNow.mockResolvedValue('agent-1')
    Object.assign(loopStore, { list: [makeLoop({ id: 'loop-42' })] })
    render(Loops)
    await fireEvent.click(screen.getByText('Run now'))
    expect(mockRunNow).toHaveBeenCalledWith('loop-42')
  })

  it('calls loopStore.remove when Delete clicked', async () => {
    mockRemove.mockResolvedValue(undefined)
    Object.assign(loopStore, { list: [makeLoop({ id: 'loop-99' })] })
    render(Loops)
    await fireEvent.click(screen.getByText('Delete'))
    expect(mockRemove).toHaveBeenCalledWith('loop-99')
  })

  it('shows loop cost when lastRunCost is set', () => {
    Object.assign(loopStore, {
      list: [makeLoop({ lastRunCost: 0.05 })],
    })
    render(Loops)
    expect(screen.getByText('cost: $0.05')).toBeDefined()
  })

  it('shows "-" for zero cost', () => {
    Object.assign(loopStore, { list: [makeLoop({ lastRunCost: 0 })] })
    render(Loops)
    expect(screen.getByText('cost: -')).toBeDefined()
  })

  it('shows "last: never" when lastRunAt is null', () => {
    Object.assign(loopStore, { list: [makeLoop({ lastRunAt: null })] })
    render(Loops)
    expect(screen.getByText('last: never')).toBeDefined()
  })
})
