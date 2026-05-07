import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, fireEvent, cleanup } from '@testing-library/svelte'
import ErrorBoundary from './ErrorBoundary.svelte'

vi.mock('../stores/notifications.svelte.js', () => ({
  notificationStore: {
    pushLocal: vi.fn(),
  },
}))

const { notificationStore } = await import('../stores/notifications.svelte.js')

// In Svelte 5, snippets are plain functions; passing a throwing function as
// children exercises the <svelte:boundary> path without needing a fixture component.
// eslint-disable-next-line @typescript-eslint/no-explicit-any
type AnyProps = any

describe('ErrorBoundary', () => {
  afterEach(() => {
    cleanup()
    vi.mocked(notificationStore.pushLocal).mockClear()
  })

  it('renders without errors when empty', () => {
    const { container } = render(ErrorBoundary, { props: {} })
    expect(container).toBeDefined()
    expect(container.textContent).not.toContain('Something went wrong')
  })

  it('shows default fallback and pushes notification when child throws', async () => {
    const throwingSnippet = () => { throw new Error('render error') }

    render(ErrorBoundary, { props: { children: throwingSnippet } as AnyProps })

    await vi.waitFor(() => {
      expect(vi.mocked(notificationStore.pushLocal)).toHaveBeenCalledWith(
        'error',
        'Component error',
        'render error',
      )
    })
    expect(screen.getByText('Something went wrong')).toBeDefined()
    expect(screen.getByRole('button', { name: 'Try again' })).toBeDefined()
  })

  it('calls onerror prop with error and reset when child throws', async () => {
    const throwingSnippet = () => { throw new Error('onerror test') }
    const onerror = vi.fn()

    render(ErrorBoundary, { props: { children: throwingSnippet, onerror } } as AnyProps)

    await vi.waitFor(() => {
      expect(onerror).toHaveBeenCalledWith(
        expect.objectContaining({ message: 'onerror test' }),
        expect.any(Function),
      )
    })
  })

  it('reset button re-renders children after error clears', async () => {
    let shouldThrow = true
    const conditionalSnippet = () => {
      if (shouldThrow) throw new Error('conditional error')
    }

    render(ErrorBoundary, { props: { children: conditionalSnippet } } as AnyProps)

    await vi.waitFor(() => {
      expect(screen.getByText('Something went wrong')).toBeDefined()
    })

    shouldThrow = false
    await fireEvent.click(screen.getByRole('button', { name: 'Try again' }))

    await vi.waitFor(() => {
      expect(screen.queryByText('Something went wrong')).toBeNull()
    })
  })
})
