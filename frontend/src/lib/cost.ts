import type { Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'

// Cumulative task cost: sums only finite, non-negative agentRuns[].costUsd —
// a null/undefined/NaN/±Infinity/negative entry (bad data, in-flight run) is
// dropped from the dollar total rather than propagating NaN or a bogus
// negative into a sum shown as money.
export function taskTotalCost(task: Pick<Task, 'agentRuns'>): number {
  const runs = task.agentRuns ?? []
  let total = 0
  for (const run of runs) {
    const cost = run.costUsd
    if (Number.isFinite(cost) && cost >= 0) total += cost
  }
  return total
}

// Every run counts, regardless of whether its cost was valid — a run with a
// missing/invalid cost still happened and still consumed agent time.
export function taskRunCount(task: Pick<Task, 'agentRuns'>): number {
  return task.agentRuns?.length ?? 0
}

// Uniform USD cost formatting for the chat surface. A single fixed precision so
// the same cumulative cost never renders as e.g. "$0.28" in the header and
// "$0.2758" in a message footer. Four decimals keeps sub-cent per-run costs
// readable instead of collapsing them to "$0.00".
export function formatCost(usd: number | null | undefined): string {
  const n = Number.isFinite(usd) ? (usd as number) : 0
  return `$${n.toFixed(4)}`
}

// Compact cost for at-a-glance lists (e.g. agent history rows) where 4 decimals
// are noise. Two decimals, with a "<$0.01" floor so a real sub-cent run never
// reads as a free "$0.00".
export function formatCostShort(usd: number | null | undefined): string {
  const n = Number.isFinite(usd) ? (usd as number) : 0
  if (n > 0 && n < 0.005) return '<$0.01'
  return `$${n.toFixed(2)}`
}
