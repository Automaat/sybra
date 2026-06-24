<script lang="ts">
  import {
    SvelteFlow,
    Controls,
    Background,
    MiniMap,
    type Node,
    type Edge,
    type NodeTypes,
  } from '@xyflow/svelte'
  import '@xyflow/svelte/dist/style.css'
  import StepNode from './nodes/StepNode.svelte'
  import EndNode from './nodes/EndNode.svelte'
  import TriggerNode from './nodes/TriggerNode.svelte'

  interface Props {
    nodes: Node[]
    edges: Edge[]
    onpositionchange?: (nodeId: string, x: number, y: number) => void
    onnodeclick?: (node: Node) => void
  }

  let { nodes, edges, onpositionchange, onnodeclick }: Props = $props()

  const nodeTypes: NodeTypes = {
    stepNode: StepNode,
    endNode: EndNode,
    triggerNode: TriggerNode,
  } as unknown as NodeTypes
</script>

<div class="wf-graph h-full w-full">
  <SvelteFlow
    {nodes}
    {edges}
    {nodeTypes}
    fitView
    onnodeclick={({ node }) => onnodeclick?.(node)}
    onnodedragstop={({ targetNode }) => {
      if (targetNode) {
        onpositionchange?.(targetNode.id, targetNode.position.x, targetNode.position.y)
      }
    }}
  >
    <Controls />
    <Background />
    <MiniMap />
  </SvelteFlow>
</div>

<style>
  /* Restyle the default white handle stubs into intentional muted connectors.
     Scoped under .wf-graph so other (future) xyflow graphs are unaffected. */
  :global(.wf-graph .svelte-flow__handle) {
    width: 8px;
    height: 8px;
    background: #94a3b8; /* slate-400 */
    border: 2px solid #fff;
    border-radius: 9999px;
  }
  :global(.dark .wf-graph .svelte-flow__handle) {
    background: #64748b; /* slate-500 */
    border-color: #1e293b; /* surface-800 */
  }
  :global(.wf-graph .svelte-flow__handle.connectingfrom),
  :global(.wf-graph .svelte-flow__handle.connectionindicator),
  :global(.wf-graph .svelte-flow__handle:hover) {
    background: #3b82f6; /* primary — clearly a connector when interacting */
  }
</style>
