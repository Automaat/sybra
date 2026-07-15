/**
 * Format elapsed time between a start ISO timestamp and a reference epoch ms.
 *
 * Returns: "0s", "59s", "1m 30s", "1h 03m"
 * Returns empty string for invalid/missing start.
 */
export function formatElapsed(startIso: string, nowMs: number): string {
  if (!startIso) return ''
  const startMs = new Date(startIso).getTime()
  if (isNaN(startMs)) return ''
  const diff = Math.max(0, Math.floor((nowMs - startMs) / 1000))
  if (diff < 60) return `${diff}s`
  if (diff < 3600) {
    const m = Math.floor(diff / 60)
    const s = diff % 60
    return `${m}m ${s}s`
  }
  const h = Math.floor(diff / 3600)
  const m = Math.floor((diff % 3600) / 60)
  return `${h}h ${String(m).padStart(2, '0')}m`
}
