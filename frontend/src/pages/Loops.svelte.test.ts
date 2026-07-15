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

  it('formats interval in days for >=86400 seconds', () => {
    Object.assign(loopStore, { list: [makeLoop({ intervalSec: 86400 })] })
    render(Loops)
    expect(screen.getByText('every 1d')).toBeDefined()
  })

  it('formats interval in minutes for sub-hour values', () => {
    Object.assign(loopStore, { list: [makeLoop({ intervalSec: 300 })] })
    render(Loops)
    expect(screen.getByText('every 5m')).toBeDefined()
  })

  it('formats interval in seconds for sub-minute values', () => {
    Object.assign(loopStore, { list: [makeLoop({ intervalSec: 30 })] })
    render(Loops)
    expect(screen.getByText('every 30s')).toBeDefined()
  })

  it('shows "just now" for very recent lastRunAt', () => {
    Object.assign(loopStore, {
      list: [makeLoop({ lastRunAt: new Date(Date.now() - 5_000).toISOString() })],
    })
    render(Loops)
    expect(screen.getByText('last: just now')).toBeDefined()
  })

  it('shows minutes-ago for recent lastRunAt', () => {
    Object.assign(loopStore, {
      list: [makeLoop({ lastRunAt: new Date(Date.now() - 5 * 60_000).toISOString() })],
    })
    render(Loops)
    expect(screen.getByText('last: 5m ago')).toBeDefined()
  })

  it('shows hours-ago for older lastRunAt', () => {
    Object.assign(loopStore, {
      list: [makeLoop({ lastRunAt: new Date(Date.now() - 3 * 3600_000).toISOString() })],
    })
    render(Loops)
    expect(screen.getByText('last: 3h ago')).toBeDefined()
  })

  it('shows days-ago for much older lastRunAt', () => {
    Object.assign(loopStore, {
      list: [makeLoop({ lastRunAt: new Date(Date.now() - 2 * 86400_000).toISOString() })],
    })
    render(Loops)
    expect(screen.getByText('last: 2d ago')).toBeDefined()
  })

  it('calls loopStore.update with toggled enabled when Pause clicked', async () => {
    mockUpdate.mockResolvedValue(undefined)
    Object.assign(loopStore, { list: [makeLoop({ id: 'lp', enabled: true })] })
    render(Loops)
    await fireEvent.click(screen.getByText('Pause'))
    expect(mockUpdate).toHaveBeenCalled()
    const arg = mockUpdate.mock.calls[0][0]
    expect(arg.enabled).toBe(false)
  })

  it('calls loopStore.update with enabled=true when Enable clicked', async () => {
    mockUpdate.mockResolvedValue(undefined)
    Object.assign(loopStore, { list: [makeLoop({ id: 'lp', enabled: false })] })
    render(Loops)
    await fireEvent.click(screen.getByText('Enable'))
    expect(mockUpdate).toHaveBeenCalled()
    const arg = mockUpdate.mock.calls[0][0]
    expect(arg.enabled).toBe(true)
  })

  it('sets store error when runNow rejects', async () => {
    mockRunNow.mockRejectedValue(new Error('run failed'))
    Object.assign(loopStore, { list: [makeLoop({ id: 'lp' })] })
    render(Loops)
    await fireEvent.click(screen.getByText('Run now'))
    await vi.waitFor(() => {
      expect(loopStore.error).toContain('run failed')
    })
  })

  it('sets store error when remove rejects', async () => {
    mockRemove.mockRejectedValue(new Error('delete failed'))
    Object.assign(loopStore, { list: [makeLoop({ id: 'lp' })] })
    render(Loops)
    await fireEvent.click(screen.getByText('Delete'))
    await vi.waitFor(() => {
      expect(loopStore.error).toContain('delete failed')
    })
  })

  it('subscribes to LoopAgentUpdated event on mount', () => {
    render(Loops)
    expect(mockEventsOn).toHaveBeenCalledWith('loopagent:updated', expect.any(Function))
  })

  it('shows model badge when set', () => {
    Object.assign(loopStore, { list: [makeLoop({ model: 'opus' })] })
    render(Loops)
    expect(screen.getByText('opus')).toBeDefined()
  })

  it('shows prompt text', () => {
    Object.assign(loopStore, { list: [makeLoop({ prompt: '/check-deploys' })] })
    render(Loops)
    expect(screen.getByText('/check-deploys')).toBeDefined()
  })

  it('renders multiple loops independently', () => {
    Object.assign(loopStore, {
      list: [
        makeLoop({ id: 'l1', name: 'one' }),
        makeLoop({ id: 'l2', name: 'two', enabled: false }),
      ],
    })
    render(Loops)
    expect(screen.getByText('one')).toBeDefined()
    expect(screen.getByText('two')).toBeDefined()
    expect(screen.getByText('active')).toBeDefined()
    expect(screen.getByText('paused')).toBeDefined()
  })

  describe('create form', () => {
    it('opens the Create modal with empty defaults when + New Loop clicked', async () => {
      render(Loops)
      await fireEvent.click(screen.getByText('+ New Loop'))
      await vi.waitFor(() => {
        expect(screen.getByText('New Loop Agent')).toBeDefined()
      })
      const nameInput = screen.getByPlaceholderText('sybra-self-monitor') as HTMLInputElement
      expect(nameInput.value).toBe('')
      const promptArea = screen.getByPlaceholderText('/sybra-self-monitor') as HTMLTextAreaElement
      expect(promptArea.value).toBe('')
    })

    it('renders Create submit button (disabled when fields empty)', async () => {
      render(Loops)
      await fireEvent.click(screen.getByText('+ New Loop'))
      await vi.waitFor(() => {
        expect(screen.getByText('New Loop Agent')).toBeDefined()
      })
      const submitBtn = screen.getByText('Create') as HTMLButtonElement
      expect(submitBtn.disabled).toBe(true)
    })

    it('closes the form when Cancel is clicked', async () => {
      render(Loops)
      await fireEvent.click(screen.getByText('+ New Loop'))
      await vi.waitFor(() => {
        expect(screen.getByText('New Loop Agent')).toBeDefined()
      })
      await fireEvent.click(screen.getByText('Cancel'))
      await vi.waitFor(() => {
        expect(screen.queryByText('New Loop Agent')).toBeNull()
      })
    })

    it('calls loopStore.create with form values when Create clicked', async () => {
      mockCreate.mockResolvedValue(undefined)
      render(Loops)
      await fireEvent.click(screen.getByText('+ New Loop'))
      await vi.waitFor(() => {
        expect(screen.getByText('New Loop Agent')).toBeDefined()
      })
      const nameInput = screen.getByPlaceholderText('sybra-self-monitor') as HTMLInputElement
      const promptArea = screen.getByPlaceholderText('/sybra-self-monitor') as HTMLTextAreaElement
      await fireEvent.input(nameInput, { target: { value: 'my-loop' } })
      await fireEvent.input(promptArea, { target: { value: '/run-thing' } })

      const submitBtn = screen.getByText('Create') as HTMLButtonElement
      await vi.waitFor(() => {
        expect(submitBtn.disabled).toBe(false)
      })
      await fireEvent.click(submitBtn)

      await vi.waitFor(() => {
        expect(mockCreate).toHaveBeenCalled()
      })
      const arg = mockCreate.mock.calls[0][0]
      expect(arg.name).toBe('my-loop')
      expect(arg.prompt).toBe('/run-thing')
      expect(arg.provider).toBe('claude')
      expect(arg.enabled).toBe(true)
      expect(arg.allowedTools).toEqual(['Bash', 'Read', 'Grep', 'Glob'])
    })

    it('shows error message when create rejects', async () => {
      mockCreate.mockRejectedValue(new Error('server down'))
      render(Loops)
      await fireEvent.click(screen.getByText('+ New Loop'))
      await vi.waitFor(() => {
        expect(screen.getByText('New Loop Agent')).toBeDefined()
      })
      const nameInput = screen.getByPlaceholderText('sybra-self-monitor') as HTMLInputElement
      const promptArea = screen.getByPlaceholderText('/sybra-self-monitor') as HTMLTextAreaElement
      await fireEvent.input(nameInput, { target: { value: 'x' } })
      await fireEvent.input(promptArea, { target: { value: 'y' } })
      await fireEvent.click(screen.getByText('Create'))
      await vi.waitFor(() => {
        expect(screen.getByText(/server down/)).toBeDefined()
      })
    })

    it('sets interval to preset value when 6h preset clicked', async () => {
      mockCreate.mockResolvedValue(undefined)
      render(Loops)
      await fireEvent.click(screen.getByText('+ New Loop'))
      await vi.waitFor(() => {
        expect(screen.getByText('New Loop Agent')).toBeDefined()
      })
      await fireEvent.click(screen.getByText('6h'))
      const nameInput = screen.getByPlaceholderText('sybra-self-monitor') as HTMLInputElement
      const promptArea = screen.getByPlaceholderText('/sybra-self-monitor') as HTMLTextAreaElement
      await fireEvent.input(nameInput, { target: { value: 'six' } })
      await fireEvent.input(promptArea, { target: { value: '/six' } })
      await fireEvent.click(screen.getByText('Create'))
      await vi.waitFor(() => {
        expect(mockCreate).toHaveBeenCalled()
      })
      expect(mockCreate.mock.calls[0][0].intervalSec).toBe(21600)
    })
  })

  describe('edit form', () => {
    it('opens Edit modal pre-populated with loop fields', async () => {
      Object.assign(loopStore, {
        list: [
          makeLoop({
            id: 'lp-99',
            name: 'existing-loop',
            prompt: '/check',
            intervalSec: 10800,
            model: 'opus',
            enabled: false,
            allowedTools: ['Bash', 'Read'],
          }),
        ],
      })
      render(Loops)
      await fireEvent.click(screen.getByText('Edit'))
      await vi.waitFor(() => {
        expect(screen.getByText('Edit Loop Agent')).toBeDefined()
      })
      const nameInput = screen.getByPlaceholderText('sybra-self-monitor') as HTMLInputElement
      const promptArea = screen.getByPlaceholderText('/sybra-self-monitor') as HTMLTextAreaElement
      expect(nameInput.value).toBe('existing-loop')
      expect(promptArea.value).toBe('/check')
      expect(screen.getByText('Update')).toBeDefined()
    })

    it('calls loopStore.update when Update clicked from edit form', async () => {
      mockUpdate.mockResolvedValue(undefined)
      Object.assign(loopStore, {
        list: [
          makeLoop({ id: 'lp-77', name: 'orig', prompt: '/p', intervalSec: 3600 }),
        ],
      })
      render(Loops)
      await fireEvent.click(screen.getByText('Edit'))
      await vi.waitFor(() => {
        expect(screen.getByText('Edit Loop Agent')).toBeDefined()
      })
      const nameInput = screen.getByPlaceholderText('sybra-self-monitor') as HTMLInputElement
      await fireEvent.input(nameInput, { target: { value: 'renamed' } })
      await fireEvent.click(screen.getByText('Update'))
      await vi.waitFor(() => {
        expect(mockUpdate).toHaveBeenCalled()
      })
      const arg = mockUpdate.mock.calls[0][0]
      expect(arg.name).toBe('renamed')
      expect(arg.id).toBe('lp-77')
    })
  })
})
