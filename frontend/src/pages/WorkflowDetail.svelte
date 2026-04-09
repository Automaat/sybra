<script lang="ts">
  import type { Node, Edge } from '@xyflow/svelte'
  import { workflowStore } from '../stores/workflows.svelte.js'
  import { workflow } from '../../wailsjs/go/models.js'
  import WorkflowGraph from '../components/workflow/WorkflowGraph.svelte'
  import StepConfigPanel from '../components/workflow/StepConfigPanel.svelte'
  import TriggerConfigPanel from '../components/workflow/TriggerConfigPanel.svelte'
  import {
    definitionToGraph,
    graphToDefinition,
    TRIGGER_NODE_ID,
    type StepNodeData,
  } from '../lib/workflow-graph.js'

  type Selection = { kind: 'step'; step: workflow.Step } | { kind: 'trigger' } | null

  interface Props {
    workflowId: string
    onback: () => void
  }

  const { workflowId, onback }: Props = $props()

  let def = $state<workflow.Definition | null>(null)
  let nodes = $state<Node[]>([])
  let edges = $state<Edge[]>([])
  let selected = $state<Selection>(null)
  let saving = $state(false)
  let dirty = $state(false)

  const selectedStep = $derived(selected?.kind === 'step' ? selected.step : null)

  const allStepIds = $derived(def?.steps?.map((s) => s.id) ?? [])

  $effect(() => {
    workflowStore.get(workflowId).then((d) => {
      def = d
      const graph = definitionToGraph(d)
      nodes = graph.nodes
      edges = graph.edges
    })
  })

  function handleNodeClick(node: Node) {
    if (node.type === 'endNode') {
      selected = null
      return
    }
    if (node.type === 'triggerNode' || node.id === TRIGGER_NODE_ID) {
      selected = { kind: 'trigger' }
      return
    }
    const data = node.data as StepNodeData
    selected = { kind: 'step', step: data.step }
  }

  function rebuildGraphFrom(updated: workflow.Definition) {
    const graph = definitionToGraph(updated)
    // Preserve current node positions from existing nodes where possible
    const posByKey = new Map<string, { x: number; y: number }>()
    for (const n of nodes) {
      posByKey.set(n.id, n.position)
    }
    nodes = graph.nodes.map((n) => {
      const p = posByKey.get(n.id)
      return p ? { ...n, position: p } : n
    })
    edges = graph.edges
  }

  function handleStepUpdate(updated: workflow.Step) {
    if (!def) return
    const idx = def.steps.findIndex((s) => s.id === selectedStep?.id)
    if (idx < 0) return
    def.steps[idx] = updated
    selected = { kind: 'step', step: updated }
    rebuildGraphFrom(def)
    dirty = true
  }

  function handleStepDelete(stepId: string) {
    if (!def) return
    def.steps = def.steps.filter((s) => s.id !== stepId)
    // Also strip any transitions that pointed to the deleted step
    for (const s of def.steps) {
      if (s.next) {
        s.next = s.next.filter((t) => t.goto !== stepId)
      }
    }
    rebuildGraphFrom(def)
    selected = null
    dirty = true
  }

  function handleStepAdd() {
    if (!def) return
    const id = `step-${crypto.randomUUID().slice(0, 8)}`
    const newStep = new workflow.Step({
      id,
      name: 'New step',
      type: 'run_agent',
      config: new workflow.StepConfig({}),
      next: [],
      parallel: [],
      position: new workflow.Position({ x: 100 + def.steps.length * 40, y: 100 + def.steps.length * 40 }),
    })
    def.steps = [...def.steps, newStep]
    rebuildGraphFrom(def)
    selected = { kind: 'step', step: newStep }
    dirty = true
  }

  function handleTriggerUpdate(trigger: workflow.Trigger) {
    if (!def) return
    def = new workflow.Definition({ ...def, trigger })
    rebuildGraphFrom(def)
    dirty = true
  }

  function handlePositionChange(nodeId: string, x: number, y: number) {
    nodes = nodes.map((n) => (n.id === nodeId ? { ...n, position: { x, y } } : n))
    dirty = true
  }

  async function save() {
    if (!def) return
    saving = true
    const updated = graphToDefinition(def, nodes, edges)
    await workflowStore.save(updated)
    def = updated
    dirty = false
    saving = false
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.metaKey && e.key === 's') {
      e.preventDefault()
      save()
    }
    if (e.key === 'Escape') {
      if (selected) {
        selected = null
      } else {
        onback()
      }
    }
  }
</script>

<svelte:window onkeydown={handleKeydown} />

<div class="flex h-full flex-col">
  <div class="flex items-center gap-3 border-b border-surface-300 px-4 py-2 dark:border-surface-600">
    <button
      type="button"
      class="rounded p-1 hover:bg-surface-200 dark:hover:bg-surface-700"
      onclick={onback}
      title="Back"
      aria-label="Back"
    >
      <svg class="h-5 w-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 19l-7-7 7-7" />
      </svg>
    </button>
    <h2 class="text-sm font-semibold">{def?.name ?? 'Loading...'}</h2>
    {#if def?.builtin}
      <span class="rounded px-1.5 py-0.5 text-xs font-medium bg-primary-100 text-primary-700 dark:bg-primary-900/40 dark:text-primary-300">built-in</span>
    {/if}
    <div class="flex-1"></div>
    {#if dirty}
      <span class="text-xs text-warning-500">unsaved</span>
    {/if}
    <button
      type="button"
      class="rounded-lg border border-surface-300 bg-surface-100 px-3 py-1.5 text-sm font-medium hover:bg-surface-200 disabled:opacity-50 dark:border-surface-600 dark:bg-surface-700 dark:hover:bg-surface-600"
      onclick={handleStepAdd}
      disabled={!def}
    >
      + Add step
    </button>
    <button
      type="button"
      class="rounded-lg bg-primary-500 px-3 py-1.5 text-sm font-medium text-white hover:bg-primary-600 disabled:opacity-50"
      onclick={save}
      disabled={saving || !dirty}
    >
      {saving ? 'Saving...' : 'Save'}
    </button>
  </div>

  <div class="flex flex-1 overflow-hidden">
    <div class="flex-1">
      {#if def}
        <WorkflowGraph
          {nodes}
          {edges}
          onpositionchange={handlePositionChange}
          onnodeclick={handleNodeClick}
        />
      {:else}
        <div class="flex h-full items-center justify-center">
          <p class="text-sm opacity-60">Loading workflow...</p>
        </div>
      {/if}
    </div>

    {#if selected?.kind === 'trigger' && def}
      <TriggerConfigPanel
        trigger={def.trigger ?? new workflow.Trigger({ on: '', conditions: [] })}
        onupdate={handleTriggerUpdate}
      />
    {:else}
      <StepConfigPanel
        step={selectedStep}
        {allStepIds}
        onupdate={handleStepUpdate}
        ondelete={handleStepDelete}
      />
    {/if}
  </div>
</div>
