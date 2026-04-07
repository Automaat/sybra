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
  } as unknown as NodeTypes
</script>

<div class="h-full w-full">
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
