import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

vi.mock('../stores/projects.svelte.js', () => ({
  projectStore: {
    get list() {
      return [
        { id: 'foo/bar', owner: 'foo', repo: 'bar' },
        { id: 'baz/qux', owner: 'baz', repo: 'qux' },
      ]
    },
  },
}))

const TaskFilterPanel = (await import('./TaskFilterPanel.svelte')).default

describe('TaskFilterPanel', () => {
  afterEach(cleanup)

  it('renders search input by default', () => {
    render(TaskFilterPanel, { props: { query: '', onqueryChange: vi.fn() } })
    expect(screen.getByPlaceholderText('Search tasks...')).toBeDefined()
  })

  it('emits onqueryChange when typing', async () => {
    const onqueryChange = vi.fn()
    render(TaskFilterPanel, { props: { query: '', onqueryChange } })
    await fireEvent.input(screen.getByTestId('filter-search'), {
      target: { value: 'auth' },
    })
    expect(onqueryChange).toHaveBeenCalledWith('auth')
  })

  it('hides project dropdown when showProject not set', () => {
    render(TaskFilterPanel, { props: { query: '', onqueryChange: vi.fn() } })
    expect(screen.queryByTestId('project-filter-button')).toBeNull()
  })

  it('shows project dropdown and emits onprojectChange', async () => {
    const onprojectChange = vi.fn()
    render(TaskFilterPanel, {
      props: {
        query: '',
        onqueryChange: vi.fn(),
        showProject: true,
        onprojectChange,
      },
    })
    await fireEvent.click(screen.getByTestId('project-filter-button'))
    await fireEvent.mouseDown(screen.getByText('foo/bar'))
    expect(onprojectChange).toHaveBeenCalledWith('foo/bar')
  })

  it('renders status pills and emits onstatusChange', async () => {
    const onstatusChange = vi.fn()
    render(TaskFilterPanel, {
      props: {
        query: '',
        onqueryChange: vi.fn(),
        statusPills: [
          { val: 'all', label: 'All' },
          { val: 'done', label: 'Done' },
        ],
        selectedStatus: 'all',
        onstatusChange,
      },
    })
    await fireEvent.click(screen.getByTestId('status-pill-done'))
    expect(onstatusChange).toHaveBeenCalledWith('done')
  })

  it('renders date range when showDateRange', async () => {
    const ondateChange = vi.fn()
    render(TaskFilterPanel, {
      props: {
        query: '',
        onqueryChange: vi.fn(),
        showDateRange: true,
        dateFrom: '',
        dateTo: '',
        ondateChange,
      },
    })
    await fireEvent.input(screen.getByTestId('date-from'), {
      target: { value: '2026-04-01' },
    })
    expect(ondateChange).toHaveBeenCalledWith('2026-04-01', '')
  })

  it('renders Clear filters when hasActive and emits onclear', async () => {
    const onclear = vi.fn()
    render(TaskFilterPanel, {
      props: { query: 'x', onqueryChange: vi.fn(), onclear, hasActive: true },
    })
    await fireEvent.click(screen.getByTestId('clear-filters'))
    expect(onclear).toHaveBeenCalled()
  })

  it('focuses search input on focusEvent', async () => {
    render(TaskFilterPanel, {
      props: { query: '', onqueryChange: vi.fn(), focusEvent: 'test-focus' },
    })
    const input = screen.getByTestId('filter-search')
    window.dispatchEvent(new CustomEvent('test-focus'))
    expect(document.activeElement).toBe(input)
  })
})
