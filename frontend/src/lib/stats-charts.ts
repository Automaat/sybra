import type { RunRecord, TaskSeriesPoint } from '../../bindings/github.com/Automaat/sybra/internal/stats/models.js'

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

export interface TimeSeriesPoint {
  date: string
  value: number
}

export const MAX_TIME_SERIES_POINTS = 366

function dayKey(d: Date): string {
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
}

function parseLocalDayKey(key: string): Date | null {
  const parts = key.split('-')
  if (parts.length !== 3) return null
  const [year, month, day] = parts.map((p) => Number(p))
  if (!Number.isInteger(year) || !Number.isInteger(month) || !Number.isInteger(day)) return null
  const d = new Date(year, month - 1, day)
  if (d.getFullYear() !== year || d.getMonth() !== month - 1 || d.getDate() !== day) return null
  return d
}

function localDaySpan(startKey: string, endKey: string): number {
  const start = parseLocalDayKey(startKey)
  const end = parseLocalDayKey(endKey)
  if (!start || !end) return 0
  const startUTC = Date.UTC(start.getFullYear(), start.getMonth(), start.getDate())
  const endUTC = Date.UTC(end.getFullYear(), end.getMonth(), end.getDate())
  return Math.floor((endUTC - startUTC) / 86_400_000) + 1
}

function capTimeSeries(points: TimeSeriesPoint[], maxPoints = MAX_TIME_SERIES_POINTS): TimeSeriesPoint[] {
  if (points.length <= maxPoints) return points
  const out: TimeSeriesPoint[] = []
  const lastIndex = points.length - 1
  let prevIndex = -1
  for (let i = 0; i < maxPoints; i += 1) {
    const index = Math.round((i * lastIndex) / (maxPoints - 1))
    if (index !== prevIndex) {
      out.push(points[index])
      prevIndex = index
    }
  }
  return out
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

export function closedTasksSeries(
  points: TaskSeriesPoint[] | null | undefined,
  cutoff: Date | null,
  end?: Date,
): TimeSeriesPoint[] {
  if (!points || points.length === 0) return []

  const buckets = new Map<string, number>()
  const cutoffKey = cutoff ? dayKey(cutoff) : null
  for (const p of points) {
    const day = parseLocalDayKey(p.date)
    if (!day) continue
    const key = dayKey(day)
    if (cutoffKey && key < cutoffKey) continue
    buckets.set(key, (buckets.get(key) ?? 0) + (p.count ?? 0))
  }

  const entries = [...buckets.entries()]
    .map(([date, value]) => ({ date, value }))
    .sort((a, b) => a.date.localeCompare(b.date))

  if (!end) return capTimeSeries(entries)

  if (buckets.size === 0) return []
  const normalizedEndKey = dayKey(end)
  const startKey = cutoffKey ?? entries[0].date
  if (startKey > normalizedEndKey) return []
  if (!cutoffKey && localDaySpan(startKey, normalizedEndKey) > MAX_TIME_SERIES_POINTS) {
    return capTimeSeries(entries)
  }

  const out: TimeSeriesPoint[] = []
  const cursor = parseLocalDayKey(startKey)
  const last = parseLocalDayKey(normalizedEndKey)
  if (!cursor || !last) return capTimeSeries(entries)
  while (cursor <= last) {
    const key = dayKey(cursor)
    out.push({ date: key, value: buckets.get(key) ?? 0 })
    cursor.setDate(cursor.getDate() + 1)
  }
  return capTimeSeries(out)
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
