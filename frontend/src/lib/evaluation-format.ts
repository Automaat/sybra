// Formatting/classification helpers shared by the Evaluation page and its
// experiment-group rendering — kept out of components so both can reuse the
// exact same rate/percent/duration formatting without drifting.

export type RateEstimateLike = {
  point?: number
  wilsonLower?: number
  wilsonUpper?: number
  deltaFromBaseline?: number
  hasDelta?: boolean
  hasData?: boolean
}

export type EstimateKey =
  | 'failureEstimate'
  | 'landedEstimate'
  | 'mergeEstimate'
  | 'ciFirstPassEstimate'
  | 'mergedWithEditsEstimate'
  | 'reworkEstimate'
  | 'revertEstimate'

export type ComparisonRowLike = {
  failureEstimate?: RateEstimateLike
  landedEstimate?: RateEstimateLike
  mergeEstimate?: RateEstimateLike
  ciFirstPassEstimate?: RateEstimateLike
  mergedWithEditsEstimate?: RateEstimateLike
  reworkEstimate?: RateEstimateLike
  revertEstimate?: RateEstimateLike
  baseline?: boolean
  baselineVariantId?: string
  sampleStatus?: string
}

export function pct(x: number | null | undefined): string {
  return Number.isFinite(x) ? `${((x ?? 0) * 100).toFixed(0)}%` : '—'
}

export function hours(x: number | undefined): string {
  return x === undefined ? '—' : `${x.toFixed(1)}h`
}

export function seconds(x: number | null | undefined): string {
  if (!Number.isFinite(x)) return '—'
  x = x ?? 0
  if (x >= 3600) return `${(x / 3600).toFixed(1)}h`
  if (x >= 60) return `${(x / 60).toFixed(1)}m`
  return `${x.toFixed(0)}s`
}

export function num(x: number | null | undefined, digits = 1): string {
  return Number.isFinite(x) ? (x ?? 0).toFixed(digits) : '—'
}

export function estimate(row: ComparisonRowLike, key: EstimateKey): RateEstimateLike | undefined {
  return row[key]
}

export function estimatePct(row: ComparisonRowLike, key: EstimateKey, fallback: number | undefined): string {
  const est = estimate(row, key)
  return est?.hasData ? pct(est.point) : pct(fallback)
}

export function estimateInterval(row: ComparisonRowLike, key: EstimateKey): string {
  const est = estimate(row, key)
  if (!est?.hasData) return ''
  return `${pct(est.wilsonLower)}-${pct(est.wilsonUpper)}`
}

export function estimateDelta(row: ComparisonRowLike, key: EstimateKey): string {
  const est = estimate(row, key)
  if (!est?.hasDelta || est.deltaFromBaseline === undefined) return ''
  const pp = est.deltaFromBaseline * 100
  return `${pp >= 0 ? '+' : ''}${pp.toFixed(0)}pp`
}

export function rateCell(row: ComparisonRowLike, key: EstimateKey, fallback: number | undefined, showDelta: boolean): string {
  const interval = estimateInterval(row, key)
  const delta = showDelta ? estimateDelta(row, key) : ''
  return [estimatePct(row, key, fallback), interval ? `CI ${interval}` : '', delta].filter(Boolean).join(' · ')
}

export function verdictClasses(verdict: string): string {
  if (verdict === 'promising') return 'bg-success-200 text-success-800 dark:bg-success-800 dark:text-success-200'
  if (verdict === 'risky') return 'bg-error-200 text-error-800 dark:bg-error-800 dark:text-error-200'
  if (verdict === 'costly') return 'bg-warning-200 text-warning-800 dark:bg-warning-800 dark:text-warning-200'
  return 'bg-surface-200 text-surface-700 dark:bg-surface-700 dark:text-surface-200'
}

export function guardrailClasses(status: string): string {
  if (status === 'breach') return 'border-error-300 text-error-700 dark:border-error-700 dark:text-error-300'
  if (status === 'watch') return 'border-warning-300 text-warning-700 dark:border-warning-700 dark:text-warning-300'
  if (status === 'ok') return 'border-success-300 text-success-700 dark:border-success-700 dark:text-success-300'
  return 'border-surface-300 text-surface-500 dark:border-surface-600 dark:text-surface-300'
}

export function sampleClasses(status: string | undefined): string {
  if (status === 'actionable') return 'bg-success-100 text-success-700 dark:bg-success-900 dark:text-success-200'
  if (status === 'directional') return 'bg-warning-100 text-warning-700 dark:bg-warning-900 dark:text-warning-200'
  if (status === 'low-sample') return 'bg-surface-200 text-surface-600 dark:bg-surface-700 dark:text-surface-200'
  return 'bg-surface-100 text-surface-500 dark:bg-surface-800 dark:text-surface-300'
}
