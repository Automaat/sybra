const KEY = 'sybra.palette.recent'
const MAX = 5

export function getRecent(): string[] {
  try {
    const raw = localStorage.getItem(KEY)
    if (!raw) return []
    const parsed: unknown = JSON.parse(raw)
    if (!Array.isArray(parsed)) return []
    return parsed.filter((x): x is string => typeof x === 'string')
  } catch {
    return []
  }
}

export function pushRecent(id: string): void {
  const current = getRecent().filter((x) => x !== id)
  const next = [id, ...current].slice(0, MAX)
  localStorage.setItem(KEY, JSON.stringify(next))
}
