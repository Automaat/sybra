import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, cleanup } from '@testing-library/svelte'

vi.mock('@skeletonlabs/skeleton-svelte', async (importOriginal) => {
  const mod = await importOriginal<typeof import('@skeletonlabs/skeleton-svelte')>()
  const fakeTab = vi.fn().mockImplementation(() => {})
  return {
    ...mod,
    Tabs: Object.assign(fakeTab, {
      List: vi.fn().mockImplementation(() => {}),
      Trigger: vi.fn().mockImplementation(({ children }: any) => children),
      Content: vi.fn().mockImplementation(() => {}),
      Indicator: vi.fn().mockImplementation(() => {}),
    }),
  }
})

vi.mock('./AgentList.svelte', () => ({ default: vi.fn().mockImplementation(() => {}) }))
vi.mock('./Orchestrator.svelte', () => ({ default: vi.fn().mockImplementation(() => {}) }))
vi.mock('./Loops.svelte', () => ({ default: vi.fn().mockImplementation(() => {}) }))

const Agents = (await import('./Agents.svelte')).default

describe('Agents', () => {
  afterEach(() => { cleanup() })

  it('renders without error with default tab', () => {
    const { container } = render(Agents, { props: { onselect: vi.fn() } })
    expect(container).toBeDefined()
  })

  it('renders without error with orchestrator tab', () => {
    const { container } = render(Agents, { props: { onselect: vi.fn(), initialTab: 'orchestrator' } })
    expect(container).toBeDefined()
  })

  it('renders without error with loops tab', () => {
    const { container } = render(Agents, { props: { onselect: vi.fn(), initialTab: 'loops' } })
    expect(container).toBeDefined()
  })
})
