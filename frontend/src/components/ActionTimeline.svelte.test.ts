import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'
import ActionTimeline from './ActionTimeline.svelte'

// jsdom does not implement scrollIntoView
beforeAll(() => {
  Element.prototype.scrollIntoView = vi.fn()
})

afterEach(() => {
  cleanup()
})

function makeEntry(overrides: Record<string, unknown> = {}) {
  return {
    index: 0,
    type: 'assistant',
    summary: 'Assistant message',
    timestamp: new Date('2026-04-01T10:00:00Z'),
    ...overrides,
  }
}

describe('ActionTimeline', () => {
  describe('empty state', () => {
    it('shows "No events yet" when entries is empty', () => {
      render(ActionTimeline, { props: { entries: [], activeIndex: null, onselect: vi.fn() } })
      expect(screen.getByText('No events yet')).toBeDefined()
    })

    it('shows entry count of 0', () => {
      render(ActionTimeline, { props: { entries: [], activeIndex: null, onselect: vi.fn() } })
      expect(screen.getByText('0')).toBeDefined()
    })
  })

  describe('with entries', () => {
    it('renders entry summaries', () => {
      const entries = [
        makeEntry({ index: 0, summary: 'Tool call: bash' }),
        makeEntry({ index: 1, summary: 'Result: success' }),
      ]
      render(ActionTimeline, { props: { entries, activeIndex: null, onselect: vi.fn() } })
      expect(screen.getByText('Tool call: bash')).toBeDefined()
      expect(screen.getByText('Result: success')).toBeDefined()
    })

    it('shows entry count', () => {
      const entries = [makeEntry({ index: 0 }), makeEntry({ index: 1 }), makeEntry({ index: 2 })]
      render(ActionTimeline, { props: { entries, activeIndex: null, onselect: vi.fn() } })
      expect(screen.getByText('3')).toBeDefined()
    })

    it('renders Timeline label', () => {
      render(ActionTimeline, { props: { entries: [makeEntry()], activeIndex: null, onselect: vi.fn() } })
      expect(screen.getByText('Timeline')).toBeDefined()
    })
  })

  describe('active entry', () => {
    it('marks active entry with aria-selected=true', () => {
      const entries = [makeEntry({ index: 0 }), makeEntry({ index: 1, summary: 'Active entry' })]
      render(ActionTimeline, { props: { entries, activeIndex: 1, onselect: vi.fn() } })
      const buttons = screen.getAllByRole('option')
      expect(buttons[0].getAttribute('aria-selected')).toBe('false')
      expect(buttons[1].getAttribute('aria-selected')).toBe('true')
    })

    it('marks all entries as not selected when activeIndex is null', () => {
      const entries = [makeEntry({ index: 0 }), makeEntry({ index: 1 })]
      render(ActionTimeline, { props: { entries, activeIndex: null, onselect: vi.fn() } })
      const buttons = screen.getAllByRole('option')
      for (const btn of buttons) {
        expect(btn.getAttribute('aria-selected')).toBe('false')
      }
    })
  })

  describe('interactions', () => {
    it('calls onselect with entry index when clicked', async () => {
      const onselect = vi.fn()
      const entries = [
        makeEntry({ index: 0, summary: 'First' }),
        makeEntry({ index: 5, summary: 'Fifth' }),
      ]
      render(ActionTimeline, { props: { entries, activeIndex: null, onselect } })
      await fireEvent.click(screen.getByText('Fifth'))
      expect(onselect).toHaveBeenCalledWith(5)
    })

    it('calls onselect when Enter key pressed on entry', async () => {
      const onselect = vi.fn()
      const entries = [makeEntry({ index: 3, summary: 'My entry' })]
      render(ActionTimeline, { props: { entries, activeIndex: null, onselect } })
      const button = screen.getByRole('option')
      await fireEvent.keyDown(button, { key: 'Enter' })
      expect(onselect).toHaveBeenCalledWith(3)
    })

    it('calls onselect when Space key pressed on entry', async () => {
      const onselect = vi.fn()
      const entries = [makeEntry({ index: 2, summary: 'My entry' })]
      render(ActionTimeline, { props: { entries, activeIndex: null, onselect } })
      const button = screen.getByRole('option')
      await fireEvent.keyDown(button, { key: ' ' })
      expect(onselect).toHaveBeenCalledWith(2)
    })

    it('does not call onselect for other keys', async () => {
      const onselect = vi.fn()
      const entries = [makeEntry({ index: 0 })]
      render(ActionTimeline, { props: { entries, activeIndex: null, onselect } })
      const button = screen.getByRole('option')
      await fireEvent.keyDown(button, { key: 'ArrowDown' })
      expect(onselect).not.toHaveBeenCalled()
    })
  })

  describe('entry types and timestamps', () => {
    it('renders formatted timestamp for each entry', () => {
      const entries = [makeEntry({ index: 0, timestamp: new Date('2026-04-01T10:05:30Z') })]
      render(ActionTimeline, { props: { entries, activeIndex: null, onselect: vi.fn() } })
      // Should contain a time string in HH:MM:SS format
      const timeEl = document.querySelector('.font-mono.tabular-nums')
      expect(timeEl?.textContent).toMatch(/\d{2}:\d{2}:\d{2}/)
    })

    it('renders dot for each entry type', () => {
      const entries = [
        makeEntry({ index: 0, type: 'assistant' }),
        makeEntry({ index: 1, type: 'tool_use' }),
        makeEntry({ index: 2, type: 'tool_result' }),
      ]
      render(ActionTimeline, { props: { entries, activeIndex: null, onselect: vi.fn() } })
      const dots = document.querySelectorAll('.rounded-full.h-2.w-2')
      expect(dots.length).toBe(3)
    })

    it('sets data-timeline-index attribute on each entry', () => {
      const entries = [makeEntry({ index: 7, summary: 'Indexed entry' })]
      render(ActionTimeline, { props: { entries, activeIndex: null, onselect: vi.fn() } })
      const button = screen.getByRole('option')
      expect(button.getAttribute('data-timeline-index')).toBe('7')
    })
  })
})
