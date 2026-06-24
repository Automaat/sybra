import { describe, it, expect } from 'vitest'
import { Definition, Trigger, Step, StepConfig, Position, Transition } from '../../bindings/github.com/Automaat/sybra/internal/workflow/models.js'
import { definitionToGraph, graphToDefinition, TRIGGER_NODE_ID } from './workflow-graph.js'

function makeDef(overrides: Record<string, unknown> = {}) {
  return Definition.createFrom({
    id: 'wf-1',
    name: 'test-workflow',
    description: '',
    trigger: Trigger.createFrom({ on: 'manual', conditions: [] }),
    steps: [],
    builtin: false,
    ...overrides,
  })
}

function makeStep(id: string, name: string, overrides: Record<string, unknown> = {}) {
  return Step.createFrom({
    id,
    name,
    type: 'run_agent',
    config: StepConfig.createFrom({}),
    next: [],
    parallel: [],
    ...overrides,
  })
}

describe('definitionToGraph', () => {
  it('returns trigger node even for empty definition', () => {
    const { nodes } = definitionToGraph(makeDef())
    const trigger = nodes.find(n => n.id === TRIGGER_NODE_ID)
    expect(trigger).toBeDefined()
    expect(trigger?.type).toBe('triggerNode')
  })

  it('uses trigger position when set', () => {
    const def = makeDef({
      trigger: Trigger.createFrom({
        on: 'manual',
        conditions: [],
        position: Position.createFrom({ x: 100, y: 200 }),
      }),
    })
    const { nodes } = definitionToGraph(def)
    const trigger = nodes.find(n => n.id === TRIGGER_NODE_ID)
    expect(trigger?.position).toEqual({ x: 100, y: 200 })
  })

  it('auto-positions the trigger when no positions are set', () => {
    const { nodes } = definitionToGraph(makeDef())
    const trigger = nodes.find(n => n.id === TRIGGER_NODE_ID)
    expect(typeof trigger?.position.x).toBe('number')
    expect(typeof trigger?.position.y).toBe('number')
  })

  it('applies a layered layout when no positions are stored', () => {
    // Trigger → s1 → s2 with no stored positions should flow top-to-bottom
    // (increasing y), not a same-row grid.
    const def = makeDef({
      steps: [
        makeStep('s1', 'Step 1', { next: [Transition.createFrom({ goto: 's2', when: null })] }),
        makeStep('s2', 'Step 2'),
      ],
    })
    const { nodes } = definitionToGraph(def)
    const trig = nodes.find(n => n.id === TRIGGER_NODE_ID)!
    const s1 = nodes.find(n => n.id === 's1')!
    const s2 = nodes.find(n => n.id === 's2')!
    expect(trig.position.y).toBeLessThan(s1.position.y)
    expect(s1.position.y).toBeLessThan(s2.position.y)
  })

  it('keeps stored positions when every node is positioned', () => {
    const def = makeDef({
      trigger: Trigger.createFrom({ on: 'manual', conditions: [], position: Position.createFrom({ x: 0, y: 0 }) }),
      steps: [makeStep('s1', 'Step 1', { position: Position.createFrom({ x: 300, y: 400 }) })],
    })
    const { nodes } = definitionToGraph(def)
    expect(nodes.find(n => n.id === 's1')?.position).toEqual({ x: 300, y: 400 })
  })

  it('creates one step node per step', () => {
    const def = makeDef({
      steps: [makeStep('s1', 'Step 1'), makeStep('s2', 'Step 2')],
    })
    const { nodes } = definitionToGraph(def)
    const stepNodes = nodes.filter(n => n.type === 'stepNode')
    expect(stepNodes).toHaveLength(2)
  })

  it('creates edge from trigger to first step', () => {
    const def = makeDef({ steps: [makeStep('s1', 'Step 1')] })
    const { edges } = definitionToGraph(def)
    const triggerEdge = edges.find(e => e.source === TRIGGER_NODE_ID)
    expect(triggerEdge).toBeDefined()
    expect(triggerEdge?.target).toBe('s1')
  })

  it('creates no trigger edge when no steps', () => {
    const { edges } = definitionToGraph(makeDef())
    const triggerEdge = edges.find(e => e.source === TRIGGER_NODE_ID)
    expect(triggerEdge).toBeUndefined()
  })

  it('creates edge for step transitions', () => {
    const step = makeStep('s1', 'Step 1', {
      next: [Transition.createFrom({ goto: 's2', when: null })],
    })
    const def = makeDef({ steps: [step, makeStep('s2', 'Step 2')] })
    const { edges } = definitionToGraph(def)
    const stepEdge = edges.find(e => e.source === 's1' && e.target === 's2')
    expect(stepEdge).toBeDefined()
  })

  it('creates end node for empty goto transition', () => {
    const step = makeStep('s1', 'Step 1', {
      next: [Transition.createFrom({ goto: '', when: null })],
    })
    const def = makeDef({ steps: [step] })
    const { nodes } = definitionToGraph(def)
    const endNode = nodes.find(n => n.type === 'endNode')
    expect(endNode).toBeDefined()
  })

  it('uses step position when every node is positioned', () => {
    const step = makeStep('s1', 'Step 1', {
      position: Position.createFrom({ x: 300, y: 400 }),
    })
    const def = makeDef({
      trigger: Trigger.createFrom({ on: 'manual', conditions: [], position: Position.createFrom({ x: 0, y: 0 }) }),
      steps: [step],
    })
    const { nodes } = definitionToGraph(def)
    const stepNode = nodes.find(n => n.id === 's1')
    expect(stepNode?.position).toEqual({ x: 300, y: 400 })
  })

  it('auto-positions a partially-positioned def (trigger set, step not)', () => {
    // Only the trigger has a position — the step must still be laid out, not
    // dropped on the grid where it could overlap.
    const step = makeStep('s1', 'Step 1')
    const def = makeDef({
      trigger: Trigger.createFrom({ on: 'manual', conditions: [], position: Position.createFrom({ x: 0, y: 0 }) }),
      steps: [step],
    })
    const { nodes } = definitionToGraph(def)
    const trig = nodes.find(n => n.id === TRIGGER_NODE_ID)!
    const s1 = nodes.find(n => n.id === 's1')!
    expect(typeof s1.position.x).toBe('number')
    // Laid out below the trigger, not stacked on it.
    expect(s1.position.y).toBeGreaterThan(trig.position.y)
  })

  it('falls back to computed position when no step position set', () => {
    const step = makeStep('s1', 'Step 1')
    const def = makeDef({ steps: [step] })
    const { nodes } = definitionToGraph(def)
    const stepNode = nodes.find(n => n.id === 's1')
    expect(stepNode?.position.x).toBeDefined()
    expect(stepNode?.position.y).toBeDefined()
  })
})

describe('graphToDefinition', () => {
  it('returns trigger from trigger node', () => {
    const original = makeDef()
    const nodes = [{ id: TRIGGER_NODE_ID, type: 'triggerNode', position: { x: 10, y: 20 }, data: {} }]
    const result = graphToDefinition(original, nodes, [])
    expect(result.trigger?.position?.x).toBe(10)
    expect(result.trigger?.position?.y).toBe(20)
  })

  it('skips end nodes', () => {
    const original = makeDef()
    const nodes = [
      { id: TRIGGER_NODE_ID, type: 'triggerNode', position: { x: 0, y: 0 }, data: {} },
      { id: '__end_s1_0', type: 'endNode', position: { x: 100, y: 100 }, data: {} },
    ]
    const result = graphToDefinition(original, nodes, [])
    expect(result.steps).toHaveLength(0)
  })

  it('converts step nodes to steps preserving position', () => {
    const step = makeStep('s1', 'Step 1')
    const original = makeDef({ steps: [step] })
    const nodes = [
      { id: TRIGGER_NODE_ID, type: 'triggerNode', position: { x: 0, y: 0 }, data: {} },
      { id: 's1', type: 'stepNode', position: { x: 200, y: 300 }, data: { step } },
    ]
    const result = graphToDefinition(original, nodes, [])
    expect(result.steps).toHaveLength(1)
    expect(result.steps[0].position?.x).toBe(200)
    expect(result.steps[0].position?.y).toBe(300)
  })

  it('preserves original definition metadata', () => {
    const original = makeDef({ name: 'my-workflow', id: 'wf-42' })
    const nodes = [{ id: TRIGGER_NODE_ID, type: 'triggerNode', position: { x: 0, y: 0 }, data: {} }]
    const result = graphToDefinition(original, nodes, [])
    expect(result.name).toBe('my-workflow')
    expect(result.id).toBe('wf-42')
  })
})
