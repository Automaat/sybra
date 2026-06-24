// Test/e2e fixture workflows (e.g. the workflow-editor.spec.ts fixture) get
// seeded into the workflow store during testing and should never appear in the
// user-facing Workflows list.

const STORAGE_KEY = 'sybra.showFixtures'

interface WorkflowLike {
  id?: string
  name?: string
}

/** True for workflows that exist only to support automated tests. */
export function isFixtureWorkflow(wf: WorkflowLike): boolean {
  const id = (wf.id ?? '').toLowerCase()
  const name = (wf.name ?? '').toLowerCase()
  // `wf-editor-e2e`, `e2e-*`, names containing "E2E" or "fixture".
  return /(^|[-_])e2e([-_]|$)/.test(id) || name.includes('e2e') || name.includes('fixture')
}

/**
 * Whether fixture workflows should be revealed anyway. Off for real users; the
 * e2e suite sets the flag so its fixture stays reachable in the list.
 */
export function showFixtures(): boolean {
  return typeof localStorage !== 'undefined' && localStorage.getItem(STORAGE_KEY) === 'true'
}
