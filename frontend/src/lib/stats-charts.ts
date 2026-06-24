import type { RunRecord } from '../../bindings/github.com/Automaat/sybra/internal/stats/models.js'

export type StatsPeriod = 'today' | 'thisWeek' | 'thisMonth' | 'allTime'

/**
 * Start-of-period cutoff matching the backend's summary boundaries
 * (internal/stats/store.go): local midnight today, the most recent Sunday for
 * the week, the first of the month. `allTime` has no cutoff.
 */
export function periodCutoff(period: StatsPeriod, now: Date): Date | null {
  const todayStart = new Date(now.getFullYear(), now.getMonth(), now.getDate())
  switch (period) {
    case 'today':
      return todayStart
    case 'thisWeek': {
      const d = new Date(todayStart)
      d.setDate(d.getDate() - todayStart.getDay())
      return d
    }
    case 'thisMonth':
      return new Date(now.getFullYear(), now.getMonth(), 1)
    default:
      return null
  }
}

function runDate(r: RunRecord): Date {
  // The binding types timestamp as time.Time, defaulting to null; `new Date(null)`
  // is the epoch (not NaN), so coalesce missing values to an invalid date.
  const ts = r.timestamp as unknown as string | null | undefined
  return ts == null ? new Date(NaN) : new Date(ts)
}

function pad(n: number): string {
  return String(n).padStart(2, '0')
}

export interface DailyCost {
  date: string
  cost: number
}

function dayKey(d: Date): string {
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
}

/**
 * Cost summed per local calendar day within the cutoff, ascending. When `end`
 * is given, every day from the range start (cutoff, or the first run) through
 * `end` is emitted — including zero-cost days — so the line is time-proportional
 * rather than just connecting the days that happen to have runs.
 */
export function dailyCost(runs: RunRecord[], cutoff: Date | null, end?: Date): DailyCost[] {
  const buckets = new Map<string, number>()
  let earliest: Date | null = null
  for (const r of runs) {
    const t = runDate(r)
    if (Number.isNaN(t.getTime())) continue
    if (cutoff && t < cutoff) continue
    buckets.set(dayKey(t), (buckets.get(dayKey(t)) ?? 0) + (r.costUsd ?? 0))
    if (!earliest || t < earliest) earliest = t
  }

  if (!end) {
    return [...buckets.entries()]
      .map(([date, cost]) => ({ date, cost }))
      .sort((a, b) => a.date.localeCompare(b.date))
  }

  if (buckets.size === 0) return []
  const startSrc = cutoff ?? earliest ?? end
  const out: DailyCost[] = []
  const cursor = new Date(startSrc.getFullYear(), startSrc.getMonth(), startSrc.getDate())
  const last = new Date(end.getFullYear(), end.getMonth(), end.getDate())
  while (cursor <= last) {
    const key = dayKey(cursor)
    out.push({ date: key, cost: buckets.get(key) ?? 0 })
    cursor.setDate(cursor.getDate() + 1)
  }
  return out
}

export interface ProjectCost {
  project: string
  cost: number
}

/** Cost summed per project within the cutoff, descending, capped at topN. */
export function costByProject(runs: RunRecord[], cutoff: Date | null, topN = 6): ProjectCost[] {
  const buckets = new Map<string, number>()
  for (const r of runs) {
    const t = runDate(r)
    if (Number.isNaN(t.getTime())) continue
    if (cutoff && t < cutoff) continue
    // Match the backend's label for project-less runs (internal/stats/store.go).
    const key = r.projectId || '(none)'
    buckets.set(key, (buckets.get(key) ?? 0) + (r.costUsd ?? 0))
  }
  return [...buckets.entries()]
    .map(([project, cost]) => ({ project, cost }))
    .filter((b) => b.cost > 0)
    .sort((a, b) => b.cost - a.cost)
    .slice(0, topN)
}
