import { describe, it, expect } from 'vitest'
import type { Node, Edge } from '@xyflow/svelte'
import { layoutGraph } from './graph-layout.js'

function node(id: string): Node {
  return { id, type: 'stepNode', position: { x: 0, y: 0 }, data: {} }
}

describe('layoutGraph', () => {
  it('lays a chain out top-to-bottom by default', () => {
    const nodes = [node('a'), node('b'), node('c')]
    const edges: Edge[] = [
      { id: 'a-b', source: 'a', target: 'b' },
      { id: 'b-c', source: 'b', target: 'c' },
    ]
    const laid = layoutGraph(nodes, edges)
    const y = (id: string) => laid.find((n) => n.id === id)!.position.y
    expect(y('a')).toBeLessThan(y('b'))
    expect(y('b')).toBeLessThan(y('c'))
  })

  it('lays out left-to-right when asked', () => {
    const nodes = [node('a'), node('b')]
    const edges: Edge[] = [{ id: 'a-b', source: 'a', target: 'b' }]
    const laid = layoutGraph(nodes, edges, 'LR')
    const x = (id: string) => laid.find((n) => n.id === id)!.position.x
    expect(x('a')).toBeLessThan(x('b'))
  })

  it('preserves node data and type', () => {
    const n = { ...node('a'), data: { label: 'keep' }, type: 'triggerNode' as const }
    const [laid] = layoutGraph([n], [])
    expect(laid.data).toEqual({ label: 'keep' })
    expect(laid.type).toBe('triggerNode')
  })

  it('ignores edges referencing missing nodes', () => {
    const nodes = [node('a')]
    const edges: Edge[] = [{ id: 'a-x', source: 'a', target: 'ghost' }]
    expect(() => layoutGraph(nodes, edges)).not.toThrow()
  })
})
