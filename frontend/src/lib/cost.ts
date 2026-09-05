import type { Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'

// A compacted task no longer carries every run it paid for. Its receipt keeps
// the cumulative totals of what was discarded, and the invariant that receipt
// exists to hold is that a later compaction never makes an earlier loss
// invisible — so both figures below read it.
export type TaskCostFields = Pick<Task, 'agentRuns' | 'documentCompaction'>

// Cumulative task cost: sums only finite, non-negative agentRuns[].costUsd —
// a null/undefined/NaN/±Infinity/negative entry (bad data, in-flight run) is
// dropped from the dollar total rather than propagating NaN or a bogus
// negative into a sum shown as money. The discarded runs' banked total is
// added on top, so a compacted task reports what it actually spent.
export function taskTotalCost(task: TaskCostFields): number {
  const runs = task.agentRuns ?? []
  let total = 0
  for (const run of runs) {
    const cost = run.costUsd
    if (Number.isFinite(cost) && cost >= 0) total += cost
  }
  const dropped = task.documentCompaction?.droppedRunCostUsd
  if (typeof dropped === 'number' && Number.isFinite(dropped) && dropped >= 0) total += dropped
  return total
}

// Every run counts, regardless of whether its cost was valid — a run with a
// missing/invalid cost still happened and still consumed agent time. A run
// discarded by compaction happened too, so the receipt's count is added — as a
// whole number, since a fraction of a run never happened.
export function taskRunCount(task: TaskCostFields): number {
  const kept = task.agentRuns?.length ?? 0
  const dropped = task.documentCompaction?.droppedAgentRuns
  if (typeof dropped !== 'number' || !Number.isInteger(dropped) || dropped <= 0) return kept
  return kept + dropped
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
