import type { task } from '../../wailsjs/go/models.js'

export type Zoom = 'day' | 'week' | 'month'

export interface TimelineDomain {
  min: Date
  max: Date
}

export interface Tick {
  date: Date
  label: string
  leftPct: number
}

export interface BarPosition {
  leftPct: number
  widthPct: number
}

const DAY_MS = 86_400_000

export function getTaskRange(t: task.Task, now = new Date()): { start: Date; end: Date } {
  const start = new Date(t.createdAt)
  let end: Date
  if (t.dueDate) {
    end = new Date(t.dueDate)
  } else if (t.status === 'done') {
    end = new Date(t.updatedAt)
  } else {
    end = now
  }
  if (end.getTime() <= start.getTime()) {
    end = new Date(start.getTime() + DAY_MS)
  }
  return { start, end }
}

export function computeTimelineDomain(tasks: task.Task[], now = new Date()): TimelineDomain {
  if (tasks.length === 0) {
    return {
      min: new Date(now.getTime() - 30 * DAY_MS),
      max: new Date(now.getTime() + 7 * DAY_MS),
    }
  }
  let minMs = Infinity
  let maxMs = -Infinity
  for (const t of tasks) {
    const { start, end } = getTaskRange(t, now)
    if (start.getTime() < minMs) minMs = start.getTime()
    if (end.getTime() > maxMs) maxMs = end.getTime()
  }
  // clamp: min = max(oldest, now-30d), max = max(latest, now+7d)
  const clampMin = Math.max(minMs, now.getTime() - 30 * DAY_MS)
  const clampMax = Math.max(maxMs, now.getTime() + 7 * DAY_MS)
  return { min: new Date(clampMin), max: new Date(clampMax) }
}

function getISOWeek(d: Date): number {
  const date = new Date(d)
  date.setHours(0, 0, 0, 0)
  date.setDate(date.getDate() + 3 - ((date.getDay() + 6) % 7))
  const week1 = new Date(date.getFullYear(), 0, 4)
  return (
    1 +
    Math.round(
      ((date.getTime() - week1.getTime()) / DAY_MS - 3 + ((week1.getDay() + 6) % 7)) / 7,
    )
  )
}

export function bucketTicks(domain: TimelineDomain, zoom: Zoom): Tick[] {
  const totalMs = domain.max.getTime() - domain.min.getTime()
  if (totalMs <= 0) return []

  const ticks: Tick[] = []
  const cursor = new Date(domain.min)

  if (zoom === 'day') {
    cursor.setHours(0, 0, 0, 0)
  } else if (zoom === 'week') {
    const day = cursor.getDay()
    cursor.setDate(cursor.getDate() - day)
    cursor.setHours(0, 0, 0, 0)
  } else {
    cursor.setDate(1)
    cursor.setHours(0, 0, 0, 0)
  }

  while (cursor.getTime() <= domain.max.getTime()) {
    const leftPct = Math.max(0, ((cursor.getTime() - domain.min.getTime()) / totalMs) * 100)
    let label: string
    if (zoom === 'day') {
      label = cursor.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
    } else if (zoom === 'week') {
      label = `W${getISOWeek(cursor)}`
    } else {
      label = cursor.toLocaleDateString(undefined, { month: 'short', year: '2-digit' })
    }
    ticks.push({ date: new Date(cursor), label, leftPct })
    if (zoom === 'day') cursor.setDate(cursor.getDate() + 1)
    else if (zoom === 'week') cursor.setDate(cursor.getDate() + 7)
    else cursor.setMonth(cursor.getMonth() + 1)
  }
  return ticks
}

export function taskBarPosition(t: task.Task, domain: TimelineDomain, now = new Date()): BarPosition {
  const { start, end } = getTaskRange(t, now)
  const totalMs = domain.max.getTime() - domain.min.getTime()
  if (totalMs <= 0) return { leftPct: 0, widthPct: 0 }
  const leftPct = Math.max(0, ((start.getTime() - domain.min.getTime()) / totalMs) * 100)
  const rightPct = Math.min(100, ((end.getTime() - domain.min.getTime()) / totalMs) * 100)
  return { leftPct, widthPct: Math.max(0.5, rightPct - leftPct) }
}

export function dueDateMarkerPosition(t: task.Task, domain: TimelineDomain): number | null {
  if (!t.dueDate) return null
  const due = new Date(t.dueDate)
  const totalMs = domain.max.getTime() - domain.min.getTime()
  if (totalMs <= 0) return null
  const pct = ((due.getTime() - domain.min.getTime()) / totalMs) * 100
  if (pct < 0 || pct > 100) return null
  return pct
}
