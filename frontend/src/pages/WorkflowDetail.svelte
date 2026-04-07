<script lang="ts">
  import type { Node, Edge } from '@xyflow/svelte'
  import { workflowStore } from '../stores/workflows.svelte.js'
  import { workflow } from '../../wailsjs/go/models.js'
  import WorkflowGraph from '../components/workflow/WorkflowGraph.svelte'
  import StepConfigPanel from '../components/workflow/StepConfigPanel.svelte'
  import { definitionToGraph, graphToDefinition, type StepNodeData } from '../lib/workflow-graph.js'

  interface Props {
    workflowId: string
    onback: () => void
  }

  const { workflowId, onback }: Props = $props()

  let def = $state<workflow.Definition | null>(null)
  let nodes = $state<Node[]>([])
  let edges = $state<Edge[]>([])
  let selectedStep = $state<workflow.Step | null>(null)
  let saving = $state(false)
  let dirty = $state(false)

  $effect(() => {
    workflowStore.get(workflowId).then(d => {
      def = d
      const graph = definitionToGraph(d)
      nodes = graph.nodes
      edges = graph.edges
    })
  })

  function handleNodeClick(node: Node) {
    if (node.type === 'endNode') {
      selectedStep = null
      return
    }
    const data = node.data as StepNodeData
    selectedStep = data.step
  }

  function handleStepUpdate(updated: workflow.Step) {
    if (!def) return
    // Update in the definition steps
    const idx = def.steps.findIndex(s => s.id === selectedStep?.id)
    if (idx >= 0) {
      def.steps[idx] = updated
      selectedStep = updated

      // Update node data
      nodes = nodes.map(n =>
        n.id === updated.id
          ? { ...n, data: { step: updated, label: updated.name || updated.id, stepType: updated.type as string } satisfies StepNodeData }
          : n
      )
      dirty = true
    }
  }

  function handleStepDelete(stepId: string) {
    if (!def) return
    def.steps = def.steps.filter(s => s.id !== stepId)
    nodes = nodes.filter(n => n.id !== stepId)
    edges = edges.filter(e => e.source !== stepId && e.target !== stepId)
    selectedStep = null
    dirty = true
  }

  function handlePositionChange(nodeId: string, x: number, y: number) {
    nodes = nodes.map(n =>
      n.id === nodeId ? { ...n, position: { x, y } } : n
    )
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
      if (selectedStep) {
        selectedStep = null
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

    <StepConfigPanel
      step={selectedStep}
      onupdate={handleStepUpdate}
      ondelete={handleStepDelete}
    />
  </div>
</div>
