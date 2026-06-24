// Uniform USD cost formatting for the chat surface. A single fixed precision so
// the same cumulative cost never renders as e.g. "$0.28" in the header and
// "$0.2758" in a message footer. Four decimals keeps sub-cent per-run costs
// readable instead of collapsing them to "$0.00".
export function formatCost(usd: number | null | undefined): string {
  const n = Number.isFinite(usd) ? (usd as number) : 0
  return `$${n.toFixed(4)}`
}
