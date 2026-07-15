import type { Component } from 'svelte'

// Memoized dynamic import for route-level code splitting. Vite emits each
// lazily-imported page as its own async chunk, so heavy/rare routes (e.g.
// WorkflowDetail, which pulls in @xyflow ~80 kB gzip) stay out of the initial
// bundle and load on first navigation instead.
//
// The returned thunk hands back the SAME promise on every call, so an
// {#await loader()} block never flashes back to its pending branch on
// re-render once the chunk has resolved.
export function lazyComponent(
  loader: () => Promise<{ default: Component<any> }>,
): () => Promise<Component<any>> {
  let cached: Promise<Component<any>> | null = null
  return () => (cached ??= loader().then((m) => m.default))
}
