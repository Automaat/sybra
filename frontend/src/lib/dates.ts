// Natural-language and ISO date helpers extracted from TaskDetail.svelte.
// Used by TaskMetadataRow due-date editor.

export function parseNaturalDate(input: string): Date | null {
  const lower = input.toLowerCase().trim()
  if (!lower || lower === 'none' || lower === 'clear') return null
  const now = new Date()
  if (lower === 'today') {
    const d = new Date(now)
    d.setHours(23, 59, 59, 0)
    return d
  }
  if (lower === 'tomorrow') {
    const d = new Date(now)
    d.setDate(d.getDate() + 1)
    d.setHours(23, 59, 59, 0)
    return d
  }
  if (lower === 'yesterday') {
    const d = new Date(now)
    d.setDate(d.getDate() - 1)
    d.setHours(23, 59, 59, 0)
    return d
  }
  const weekdays = ['sunday', 'monday', 'tuesday', 'wednesday', 'thursday', 'friday', 'saturday']
  const nextMatch = lower.match(/^(?:next\s+)?(\w+)$/)
  if (nextMatch) {
    const day = weekdays.indexOf(nextMatch[1])
    if (day !== -1) {
      const d = new Date(now)
      const current = d.getDay()
      const diff = ((day - current + 7) % 7) || 7
      d.setDate(d.getDate() + diff)
      d.setHours(23, 59, 59, 0)
      return d
    }
  }
  const inMatch = lower.match(/^in\s+(\d+)\s+(day|days|week|weeks)$/)
  if (inMatch) {
    const n = parseInt(inMatch[1])
    const d = new Date(now)
    d.setDate(d.getDate() + (inMatch[2].startsWith('week') ? n * 7 : n))
    d.setHours(23, 59, 59, 0)
    return d
  }
  const parsed = new Date(input)
  return isNaN(parsed.getTime()) ? null : parsed
}

export function formatDueDateDisplay(date: unknown): string {
  if (!date) return 'Set due date'
  const d = new Date(date as string | number | Date)
  if (isNaN(d.getTime())) return 'Set due date'
  const now = new Date()
  const today = new Date(now.getFullYear(), now.getMonth(), now.getDate())
  const target = new Date(d.getFullYear(), d.getMonth(), d.getDate())
  const diff = Math.round((target.getTime() - today.getTime()) / 86400000)
  if (diff === 0) return 'Today'
  if (diff === 1) return 'Tomorrow'
  if (diff === -1) return 'Yesterday'
  if (diff > 1 && diff < 7) return `In ${diff} days`
  if (diff < 0) return `${Math.abs(diff)}d overdue`
  return d.toLocaleDateString(undefined, {
    month: 'short',
    day: 'numeric',
    year: d.getFullYear() !== now.getFullYear() ? 'numeric' : undefined,
  })
}

export function formatDate(date: unknown): string {
  if (!date) return '-'
  return new Date(date as string | number | Date).toLocaleString()
}

// UTC-anchored day boundaries for date-range filtering. Used by Logbook.
export function toUtcDayStart(dateStr: string): Date | null {
  if (!dateStr) return null
  const [y, m, d] = dateStr.split('-').map(Number)
  if (!y || !m || !d) return null
  return new Date(Date.UTC(y, m - 1, d, 0, 0, 0, 0))
}

export function toUtcDayEnd(dateStr: string): Date | null {
  if (!dateStr) return null
  const [y, m, d] = dateStr.split('-').map(Number)
  if (!y || !m || !d) return null
  return new Date(Date.UTC(y, m - 1, d, 23, 59, 59, 999))
}
