import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'
import StatusPicker from './StatusPicker.svelte'
import { STATUS_OPTIONS } from '../lib/statuses.js'

afterEach(() => {
  cleanup()
})

describe('StatusPicker', () => {
  it('renders all status options', () => {
    render(StatusPicker, { props: { currentStatus: 'todo', onpick: vi.fn(), onclose: vi.fn() } })
    for (const opt of STATUS_OPTIONS) {
      expect(screen.getByText(opt.label)).toBeDefined()
    }
  })

  it('shows "current" label next to the active status', () => {
    render(StatusPicker, { props: { currentStatus: 'in-progress', onpick: vi.fn(), onclose: vi.fn() } })
    const currentLabels = screen.getAllByText('current')
    expect(currentLabels).toHaveLength(1)
    const inProgressBtn = screen.getByText('In Progress').closest('button')
    expect(inProgressBtn?.textContent).toContain('current')
  })

  it('calls onpick with status value when option clicked', async () => {
    const onpick = vi.fn()
    render(StatusPicker, { props: { currentStatus: 'todo', onpick, onclose: vi.fn() } })
    await fireEvent.click(screen.getByText('In Progress'))
    expect(onpick).toHaveBeenCalledWith('in-progress')
  })

  it('calls onclose when backdrop clicked', async () => {
    const onclose = vi.fn()
    render(StatusPicker, { props: { currentStatus: 'todo', onpick: vi.fn(), onclose } })
    const backdrop = document.querySelector('.fixed.inset-0')
    await fireEvent.click(backdrop!)
    expect(onclose).toHaveBeenCalled()
  })

  it('calls onclose when Escape key pressed', async () => {
    const onclose = vi.fn()
    render(StatusPicker, { props: { currentStatus: 'todo', onpick: vi.fn(), onclose } })
    await fireEvent.keyDown(window, { key: 'Escape' })
    expect(onclose).toHaveBeenCalled()
  })

  it('navigates down with ArrowDown key from first option', async () => {
    const onpick = vi.fn()
    // Start at index 0 ('new') so ArrowDown goes to index 1
    render(StatusPicker, { props: { currentStatus: STATUS_OPTIONS[0].value, onpick, onclose: vi.fn() } })
    await fireEvent.keyDown(window, { key: 'ArrowDown' })
    await fireEvent.keyDown(window, { key: 'Enter' })
    expect(onpick).toHaveBeenCalledWith(STATUS_OPTIONS[1].value)
  })

  it('navigates down with j key', async () => {
    const onpick = vi.fn()
    // Start at index 0 ('new') so j goes to index 1
    render(StatusPicker, { props: { currentStatus: STATUS_OPTIONS[0].value, onpick, onclose: vi.fn() } })
    await fireEvent.keyDown(window, { key: 'j' })
    await fireEvent.keyDown(window, { key: 'Enter' })
    expect(onpick).toHaveBeenCalledWith(STATUS_OPTIONS[1].value)
  })

  it('navigates up with ArrowUp key and does not go below 0', async () => {
    const onpick = vi.fn()
    render(StatusPicker, { props: { currentStatus: STATUS_OPTIONS[0].value, onpick, onclose: vi.fn() } })
    await fireEvent.keyDown(window, { key: 'ArrowUp' })
    await fireEvent.keyDown(window, { key: 'Enter' })
    expect(onpick).toHaveBeenCalledWith(STATUS_OPTIONS[0].value)
  })

  it('navigates up with k key', async () => {
    const onpick = vi.fn()
    // Start at index 2 so k goes to index 1
    render(StatusPicker, { props: { currentStatus: STATUS_OPTIONS[2].value, onpick, onclose: vi.fn() } })
    await fireEvent.keyDown(window, { key: 'k' })
    await fireEvent.keyDown(window, { key: 'Enter' })
    expect(onpick).toHaveBeenCalledWith(STATUS_OPTIONS[1].value)
  })

  it('does not navigate past last option with ArrowDown', async () => {
    const onpick = vi.fn()
    const lastStatus = STATUS_OPTIONS[STATUS_OPTIONS.length - 1].value
    render(StatusPicker, { props: { currentStatus: lastStatus, onpick, onclose: vi.fn() } })
    for (let i = 0; i < 5; i++) {
      await fireEvent.keyDown(window, { key: 'ArrowDown' })
    }
    await fireEvent.keyDown(window, { key: 'Enter' })
    expect(onpick).toHaveBeenCalledWith(lastStatus)
  })

  it('selects current option with Enter key', async () => {
    const onpick = vi.fn()
    // Start at first option ('new')
    render(StatusPicker, { props: { currentStatus: STATUS_OPTIONS[0].value, onpick, onclose: vi.fn() } })
    await fireEvent.keyDown(window, { key: 'Enter' })
    expect(onpick).toHaveBeenCalledWith(STATUS_OPTIONS[0].value)
  })
})
