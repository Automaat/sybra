import dagre from '@dagrejs/dagre'
import type { Node, Edge } from '@xyflow/svelte'

// Fallback node dimensions used before xyflow measures the real DOM nodes.
const NODE_W = 180
const NODE_H = 72

export type LayoutDirection = 'TB' | 'LR'

/**
 * Assign layered DAG positions to graph nodes with dagre, so edges flow in one
 * consistent direction instead of crisscrossing. Returns new node objects with
 * updated `position`; node data/type are preserved. Default direction is
 * top-to-bottom (`TB`).
 */
export function layoutGraph(nodes: Node[], edges: Edge[], direction: LayoutDirection = 'TB'): Node[] {
  const g = new dagre.graphlib.Graph()
  g.setDefaultEdgeLabel(() => ({}))
  g.setGraph({ rankdir: direction, nodesep: 60, ranksep: 90 })

  for (const n of nodes) {
    g.setNode(n.id, { width: n.width ?? NODE_W, height: n.height ?? NODE_H })
  }
  for (const e of edges) {
    // Only lay out edges whose endpoints are present as nodes.
    if (g.hasNode(e.source) && g.hasNode(e.target)) g.setEdge(e.source, e.target)
  }

  dagre.layout(g)

  return nodes.map((n) => {
    const laid = g.node(n.id)
    if (!laid) return n
    const w = n.width ?? NODE_W
    const h = n.height ?? NODE_H
    // dagre returns the node center; xyflow positions are the top-left corner.
    return { ...n, position: { x: laid.x - w / 2, y: laid.y - h / 2 } }
  })
}
