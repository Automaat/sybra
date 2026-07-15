import { describe, it, expect, vi, beforeEach, beforeAll } from 'vitest'
import { render } from '@testing-library/svelte'

const mockCreate = vi.fn()

vi.mock('../stores/projects.svelte.js', () => ({
  projectStore: {
    create: (...args: unknown[]) => mockCreate(...args),
  },
}))

beforeAll(() => {
  globalThis.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  } as unknown as typeof ResizeObserver
})

const CreateProjectDialog = (await import('./CreateProjectDialog.svelte')).default

describe('CreateProjectDialog', () => {
  beforeEach(() => {
    vi.clearAllMocks()
  })

  it('renders without errors when open=false', () => {
    const { container } = render(CreateProjectDialog, {
      props: { open: false, onOpenChange: vi.fn() },
    })
    expect(container).toBeDefined()
  })

  it('renders without errors when open=true', () => {
    const { container } = render(CreateProjectDialog, {
      props: { open: true, onOpenChange: vi.fn() },
    })
    expect(container).toBeDefined()
  })

  it('accepts optional oncreated prop', () => {
    const { container } = render(CreateProjectDialog, {
      props: { open: false, onOpenChange: vi.fn(), oncreated: vi.fn() },
    })
    expect(container).toBeDefined()
  })
})
