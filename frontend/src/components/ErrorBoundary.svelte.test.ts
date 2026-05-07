import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, cleanup } from '@testing-library/svelte'
import ErrorBoundary from './ErrorBoundary.svelte'

vi.mock('../stores/notifications.svelte.js', () => ({
  notificationStore: {
    pushLocal: vi.fn(),
  },
}))

describe('ErrorBoundary', () => {
  afterEach(cleanup)

  it('renders without errors', () => {
    const { container } = render(ErrorBoundary, {
      props: {},
    })
    expect(container).toBeDefined()
  })
})
