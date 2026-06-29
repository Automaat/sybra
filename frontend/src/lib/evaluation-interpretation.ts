import type { ComparisonBreakdown } from '../../bindings/github.com/Automaat/sybra/internal/evaluation/models.js'

export type ExperimentVerdict = 'underpowered' | 'needs-more-data' | 'risky' | 'costly' | 'promising'

export type GuardrailStatus = 'ok' | 'watch' | 'breach' | 'limited'
export type GuardrailCategory = 'risk' | 'cost'

export interface GuardrailSignal {
  key: string
  label: string
  category: GuardrailCategory
  status: GuardrailStatus
  detail: string
}

export interface ExperimentInterpretation {
  verdict: ExperimentVerdict
  verdictLabel: string
  verdictReason: string
  primaryLabel: string
  primaryValue: string
  primaryDetail: string
  guardrails: GuardrailSignal[]
  limitedSignals: string[]
}

const verdictLabels: Record<ExperimentVerdict, string> = {
  underpowered: 'Underpowered',
  'needs-more-data': 'Needs more data',
  risky: 'Risky',
  costly: 'Costly',
  promising: 'Promising',
}

interface PeerMetric {
  key: 'durationP90S' | 'costPerLanded' | 'premiumRequestsPerLanded'
  label: string
  value: number | undefined
  unit: string
  format: (value: number) => string
}

export function interpretExperiment(
  row: ComparisonBreakdown,
  peers: ComparisonBreakdown[] = [],
): ExperimentInterpretation {
  const runs = finiteNonNegative(row.runs)
  const landed = finiteNonNegative(row.landed)
  const landedPerRun = runs > 0 ? landed / runs : undefined
  const guardrails: GuardrailSignal[] = [
    thresholdLow('ci-first-pass', 'CI first pass', 'risk', finiteRate(row.ciFirstPassRate), 0.6, 0.8),
    thresholdHigh('edited-merge', 'Edited merge rate', 'risk', finiteRate(row.mergedWithEditsRate), 0.3, 0.15),
    thresholdHigh('rework', 'Rework', 'risk', finiteRate(row.reworkRate), 0.3, 0.15),
    thresholdHigh('revert', 'Revert', 'risk', finiteRate(row.revertRate), 0, 0),
  ]
  const limitedSignals: string[] = []

  const peerMetrics: PeerMetric[] = [
    {
      key: 'durationP90S',
      label: 'Duration p90',
      value: finitePositive(row.durationP90S),
      unit: 'peer median',
      format: formatSeconds,
    },
    {
      key: 'costPerLanded',
      label: 'Cost per landed',
      value: finitePositive(row.costPerLanded),
      unit: 'peer median',
      format: (value) => `$${value.toFixed(2)}`,
    },
    {
      key: 'premiumRequestsPerLanded',
      label: 'Premium requests',
      value: finitePositive(row.premiumRequestsPerLanded),
      unit: 'peer median',
      format: (value) => `${value.toFixed(1)}/landed`,
    },
  ]

  for (const metric of peerMetrics) {
    const peerMedian = median(peerValues(row, peers, metric.key))
    if (peerMedian === undefined) {
      guardrails.push({
        key: metric.key,
        label: metric.label,
        category: 'cost',
        status: 'limited',
        detail: `No usable same-experiment ${metric.unit}.`,
      })
      limitedSignals.push(`${metric.label} has no usable same-experiment peer baseline.`)
      continue
    }

    const value = metric.value
    if (value === undefined) {
      guardrails.push({
        key: metric.key,
        label: metric.label,
        category: 'cost',
        status: 'limited',
        detail: `No positive ${metric.label.toLowerCase()} value for this variant.`,
      })
      limitedSignals.push(`${metric.label} is unavailable for this variant.`)
      continue
    }

    const ratio = value / peerMedian
    const status: GuardrailStatus = ratio >= 1.5 ? 'breach' : ratio >= 1.25 ? 'watch' : 'ok'
    guardrails.push({
      key: metric.key,
      label: metric.label,
      category: 'cost',
      status,
      detail: `${metric.format(value)} vs ${metric.format(peerMedian)} ${metric.unit} (${ratio.toFixed(2)}x).`,
    })
  }

  if (row.qualityAttributionLimited) {
    limitedSignals.push('Quality attribution is limited because this variant has runs but no landed tasks.')
  }

  const verdict = deriveVerdict(row, landed, guardrails)
  return {
    verdict,
    verdictLabel: verdictLabels[verdict],
    verdictReason: verdictReason(verdict, guardrails, row.qualityAttributionLimited, landed),
    primaryLabel: 'Landed/run',
    primaryValue: landedPerRun === undefined ? '—' : `${(landedPerRun * 100).toFixed(0)}%`,
    primaryDetail: `${landed}/${runs} landed`,
    guardrails,
    limitedSignals,
  }
}

function deriveVerdict(row: ComparisonBreakdown, landed: number, guardrails: GuardrailSignal[]): ExperimentVerdict {
  if (row.insufficientData) return 'underpowered'
  if (row.qualityAttributionLimited || landed === 0) return 'needs-more-data'
  if (guardrails.some((g) => g.category === 'risk' && g.status === 'breach')) return 'risky'
  if (guardrails.some((g) => g.category === 'cost' && g.status === 'breach')) return 'costly'
  return 'promising'
}

function verdictReason(
  verdict: ExperimentVerdict,
  guardrails: GuardrailSignal[],
  qualityAttributionLimited: boolean,
  landed: number,
): string {
  if (verdict === 'underpowered') return 'Run count is below the comparison minimum.'
  if (qualityAttributionLimited) return 'Runs exist, but landed-task quality signals are not attributable yet.'
  if (landed === 0) return 'No landed tasks yet, so outcome quality cannot be judged.'
  const breach = guardrails.find((g) => g.status === 'breach')
  if (breach) return `${breach.label} breached its guardrail.`
  return 'Primary signal is available and no guardrail is breached.'
}

function thresholdLow(
  key: string,
  label: string,
  category: GuardrailCategory,
  value: number | undefined,
  breachBelow: number,
  watchBelow: number,
): GuardrailSignal {
  if (value === undefined) return { key, label, category, status: 'limited', detail: 'No finite rate available.' }
  const status: GuardrailStatus = value < breachBelow ? 'breach' : value < watchBelow ? 'watch' : 'ok'
  return { key, label, category, status, detail: `${formatPct(value)} (${formatPct(breachBelow)} breach floor).` }
}

function thresholdHigh(
  key: string,
  label: string,
  category: GuardrailCategory,
  value: number | undefined,
  breachAbove: number,
  watchAbove: number,
): GuardrailSignal {
  if (value === undefined) return { key, label, category, status: 'limited', detail: 'No finite rate available.' }
  const status: GuardrailStatus = value > breachAbove ? 'breach' : value > watchAbove ? 'watch' : 'ok'
  return { key, label, category, status, detail: `${formatPct(value)} (${formatPct(breachAbove)} breach ceiling).` }
}

function peerValues(
  row: ComparisonBreakdown,
  peers: ComparisonBreakdown[],
  key: PeerMetric['key'],
): number[] {
  return peers
    .filter((peer) => {
      if (peer.key === row.key) return false
      if (peer.insufficientData) return false
      if (peer.experimentId !== row.experimentId) return false
      if (row.role && peer.role !== row.role) return false
      return true
    })
    .map((peer) => finitePositive(peer[key]))
    .filter((value): value is number => value !== undefined)
}

function median(values: number[]): number | undefined {
  if (values.length === 0) return undefined
  const sorted = [...values].sort((a, b) => a - b)
  const mid = Math.floor(sorted.length / 2)
  if (sorted.length % 2 === 1) return sorted[mid]
  return (sorted[mid - 1] + sorted[mid]) / 2
}

function finiteNonNegative(value: number | null | undefined): number {
  return Number.isFinite(value) && value !== null && value !== undefined && value > 0 ? value : 0
}

function finitePositive(value: number | null | undefined): number | undefined {
  return Number.isFinite(value) && value !== null && value !== undefined && value > 0 ? value : undefined
}

function finiteRate(value: number | null | undefined): number | undefined {
  return Number.isFinite(value) && value !== null && value !== undefined ? Math.max(0, value) : undefined
}

function formatPct(value: number): string {
  return `${(value * 100).toFixed(0)}%`
}

function formatSeconds(value: number): string {
  if (value >= 3600) return `${(value / 3600).toFixed(1)}h`
  if (value >= 60) return `${(value / 60).toFixed(1)}m`
  return `${value.toFixed(0)}s`
}
