import type { Node, Edge } from '@xyflow/svelte'
import { workflow } from '../../wailsjs/go/models.js'

const NODE_SPACING_X = 250
const NODE_SPACING_Y = 120

type StepNodeData = {
  step: workflow.Step
  label: string
  stepType: string
}

const stepTypeColors: Record<string, string> = {
  run_agent: '#3b82f6',   // blue
  wait_human: '#f59e0b',  // amber
  set_status: '#10b981',  // green
  condition: '#8b5cf6',   // purple
  shell: '#6b7280',       // gray
  parallel: '#ec4899',    // pink
}

export function definitionToGraph(def: workflow.Definition): { nodes: Node[], edges: Edge[] } {
  const nodes: Node[] = []
  const edges: Edge[] = []
  const steps = def.steps ?? []

  for (let i = 0; i < steps.length; i++) {
    const step = steps[i]
    const pos = step.position
      ? { x: step.position.x, y: step.position.y }
      : { x: NODE_SPACING_X * (i % 4), y: NODE_SPACING_Y * Math.floor(i / 4) }

    nodes.push({
      id: step.id,
      type: 'stepNode',
      position: pos,
      data: {
        step,
        label: step.name || step.id,
        stepType: step.type as string,
      } satisfies StepNodeData,
    })

    const transitions = step.next ?? []
    for (let j = 0; j < transitions.length; j++) {
      const t = transitions[j]
      if (!t.goto && t.goto !== '') continue
      if (t.goto === '') {
        // End node — create a virtual end node
        const endId = `__end_${step.id}_${j}`
        if (!nodes.find(n => n.id === endId)) {
          nodes.push({
            id: endId,
            type: 'endNode',
            position: { x: pos.x + NODE_SPACING_X, y: pos.y + j * 60 },
            data: { label: 'End' },
          })
        }
        edges.push({
          id: `${step.id}->${endId}`,
          source: step.id,
          target: endId,
          label: t.when ? formatCondition(t.when) : '',
          animated: !t.when,
        })
      } else {
        edges.push({
          id: `${step.id}->${t.goto}`,
          source: step.id,
          target: t.goto,
          label: t.when ? formatCondition(t.when) : '',
          animated: !t.when,
        })
      }
    }
  }

  return { nodes, edges }
}

export function graphToDefinition(
  original: workflow.Definition,
  nodes: Node[],
  _edges: Edge[],
): workflow.Definition {
  // Transitions are authoritative on step.next (edited via StepConfigPanel).
  // Edges are a visual projection only; we rebuild them on load via definitionToGraph.
  const steps: workflow.Step[] = []

  for (const node of nodes) {
    if (node.type === 'endNode') continue

    const data = node.data as StepNodeData
    const src = data.step

    steps.push(new workflow.Step({
      ...src,
      position: new workflow.Position({ x: node.position.x, y: node.position.y }),
      next: src.next ?? [],
    }))
  }

  return new workflow.Definition({
    ...original,
    steps,
  })
}

function formatCondition(c: workflow.Condition): string {
  if (!c) return ''
  const field = c.field?.split('.').pop() ?? c.field
  return `${field} ${c.operator} ${c.value}`
}

export { stepTypeColors }
export type { StepNodeData }
