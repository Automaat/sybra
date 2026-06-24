import { describe, it, expect, vi, beforeEach, beforeAll } from 'vitest'
import { render } from '@testing-library/svelte'
import CreateTaskDialog from './CreateTaskDialog.svelte'

vi.mock('../stores/tasks.svelte.js', () => ({
  taskStore: {
    create: vi.fn(),
  },
}))


beforeAll(() => {
  // Skeleton UI's SegmentedControl uses @zag-js which requires ResizeObserver
  globalThis.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  } as unknown as typeof ResizeObserver
})

describe('CreateTaskDialog', () => {
  beforeEach(() => {
    vi.clearAllMocks()
  })

  it('renders without errors when open=false', () => {
    const { container } = render(CreateTaskDialog, {
      props: { open: false, onOpenChange: vi.fn() },
    })
    expect(container).toBeDefined()
  })

  it('renders without errors when open=true', () => {
    const { container } = render(CreateTaskDialog, {
      props: { open: true, onOpenChange: vi.fn() },
    })
    expect(container).toBeDefined()
  })

  it('accepts optional oncreated prop', () => {
    const oncreated = vi.fn()
    const { container } = render(CreateTaskDialog, {
      props: { open: false, onOpenChange: vi.fn(), oncreated },
    })
    expect(container).toBeDefined()
  })

  it('marks the required title and explains the Type field', () => {
    const { container } = render(CreateTaskDialog, {
      props: { open: true, onOpenChange: vi.fn() },
    })
    // The required marker sits on the Title field, next to its required input,
    // with a screen-reader-only "(required)" so it isn't visual-only.
    const titleLabel = Array.from(container.querySelectorAll('label')).find((l) =>
      l.textContent?.includes('Title'),
    )
    expect(titleLabel?.querySelector('input[required]')).not.toBeNull()
    expect(titleLabel?.querySelector('.sr-only')?.textContent).toContain('required')
    // One-line Type helper.
    expect(container.textContent).toContain('Sets how the agent runs')
  })
})
