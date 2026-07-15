import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'
import PriorityPicker from './PriorityPicker.svelte'
import { PRIORITY_OPTIONS } from '../lib/priorities.js'

afterEach(() => {
  cleanup()
})

describe('PriorityPicker', () => {
  it('renders all priority options', () => {
    render(PriorityPicker, { props: { currentPriority: '', onpick: vi.fn(), onclose: vi.fn() } })
    for (const opt of PRIORITY_OPTIONS) {
      expect(screen.getByText(opt.label)).toBeDefined()
    }
  })

  it('shows "current" label next to the active priority', () => {
    render(PriorityPicker, { props: { currentPriority: 'high', onpick: vi.fn(), onclose: vi.fn() } })
    const currentLabels = screen.getAllByText('current')
    expect(currentLabels).toHaveLength(1)
    const highBtn = screen.getByText('High').closest('button')
    expect(highBtn?.textContent).toContain('current')
  })

  it('calls onpick with priority value when option clicked', async () => {
    const onpick = vi.fn()
    render(PriorityPicker, { props: { currentPriority: '', onpick, onclose: vi.fn() } })
    await fireEvent.click(screen.getByText('High'))
    expect(onpick).toHaveBeenCalledWith('high')
  })

  it('calls onpick with empty string when None clicked', async () => {
    const onpick = vi.fn()
    render(PriorityPicker, { props: { currentPriority: 'high', onpick, onclose: vi.fn() } })
    await fireEvent.click(screen.getByText('None'))
    expect(onpick).toHaveBeenCalledWith('')
  })

  it('calls onclose when backdrop clicked', async () => {
    const onclose = vi.fn()
    render(PriorityPicker, { props: { currentPriority: '', onpick: vi.fn(), onclose } })
    const backdrop = document.querySelector('.fixed.inset-0')
    await fireEvent.click(backdrop!)
    expect(onclose).toHaveBeenCalled()
  })

  it('calls onclose when Escape pressed', async () => {
    const onclose = vi.fn()
    render(PriorityPicker, { props: { currentPriority: '', onpick: vi.fn(), onclose } })
    await fireEvent.keyDown(window, { key: 'Escape' })
    expect(onclose).toHaveBeenCalled()
  })

  it('navigates down with ArrowDown and selects with Enter', async () => {
    const onpick = vi.fn()
    render(PriorityPicker, { props: { currentPriority: '', onpick, onclose: vi.fn() } })
    await fireEvent.keyDown(window, { key: 'ArrowDown' })
    await fireEvent.keyDown(window, { key: 'Enter' })
    expect(onpick).toHaveBeenCalledWith(PRIORITY_OPTIONS[1].value)
  })

  it('navigates down with j key', async () => {
    const onpick = vi.fn()
    render(PriorityPicker, { props: { currentPriority: '', onpick, onclose: vi.fn() } })
    await fireEvent.keyDown(window, { key: 'j' })
    await fireEvent.keyDown(window, { key: 'Enter' })
    expect(onpick).toHaveBeenCalledWith(PRIORITY_OPTIONS[1].value)
  })

  it('navigates up with ArrowUp and does not go below 0', async () => {
    const onpick = vi.fn()
    render(PriorityPicker, { props: { currentPriority: '', onpick, onclose: vi.fn() } })
    await fireEvent.keyDown(window, { key: 'ArrowUp' })
    await fireEvent.keyDown(window, { key: 'Enter' })
    expect(onpick).toHaveBeenCalledWith(PRIORITY_OPTIONS[0].value)
  })

  it('navigates up with k key', async () => {
    const onpick = vi.fn()
    render(PriorityPicker, { props: { currentPriority: PRIORITY_OPTIONS[2].value, onpick, onclose: vi.fn() } })
    await fireEvent.keyDown(window, { key: 'k' })
    await fireEvent.keyDown(window, { key: 'Enter' })
    expect(onpick).toHaveBeenCalledWith(PRIORITY_OPTIONS[1].value)
  })

  it('does not navigate past last option', async () => {
    const onpick = vi.fn()
    const lastPriority = PRIORITY_OPTIONS[PRIORITY_OPTIONS.length - 1].value
    render(PriorityPicker, { props: { currentPriority: lastPriority, onpick, onclose: vi.fn() } })
    for (let i = 0; i < 5; i++) {
      await fireEvent.keyDown(window, { key: 'ArrowDown' })
    }
    await fireEvent.keyDown(window, { key: 'Enter' })
    expect(onpick).toHaveBeenCalledWith(lastPriority)
  })

  it('renders priority icons', () => {
    render(PriorityPicker, { props: { currentPriority: '', onpick: vi.fn(), onclose: vi.fn() } })
    expect(screen.getByText('↑')).toBeDefined()
    expect(screen.getByText('↓')).toBeDefined()
    expect(screen.getByText('→')).toBeDefined()
  })
})
