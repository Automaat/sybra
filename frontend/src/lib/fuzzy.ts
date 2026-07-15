/**
 * Fuzzy string scorer for the command palette.
 *
 * score(query, target):
 *   - Returns null if any query character is unmatched (subsequence test fails).
 *   - Returns 0 for an empty query.
 *   - Higher score = better match.
 *
 * Scoring weights:
 *   +10 per matched character (base)
 *   +15 consecutive-match bonus (match immediately follows previous match)
 *   +10 word-boundary bonus (match at position 0, or after whitespace/hyphen/underscore/slash)
 *   −3  per gap character between matches
 */
export function score(query: string, target: string): number | null {
  if (!query) return 0
  const q = query.toLowerCase()
  const t = target.toLowerCase()

  let qi = 0
  let prevMatchIdx = -2 // sentinel: no previous match yet
  let sc = 0

  for (let ti = 0; ti < t.length && qi < q.length; ti++) {
    if (t[ti] !== q[qi]) continue
    sc += 10
    if (prevMatchIdx >= 0 && prevMatchIdx === ti - 1) sc += 15
    if (ti === 0 || /[\s\-_/]/.test(t[ti - 1])) sc += 10
    if (prevMatchIdx >= 0) sc -= 3 * (ti - prevMatchIdx - 1)
    prevMatchIdx = ti
    qi++
  }

  return qi < q.length ? null : sc
}

/**
 * Returns [start, end) ranges of matched characters in target for highlighting.
 * Returns null if no match.
 */
export function matchRanges(query: string, target: string): [number, number][] | null {
  if (!query) return []
  const q = query.toLowerCase()
  const t = target.toLowerCase()

  const positions: number[] = []
  let qi = 0

  for (let ti = 0; ti < t.length && qi < q.length; ti++) {
    if (t[ti] === q[qi]) {
      positions.push(ti)
      qi++
    }
  }

  if (qi < q.length) return null

  const ranges: [number, number][] = []
  let start = positions[0]
  let end = positions[0] + 1
  for (let i = 1; i < positions.length; i++) {
    if (positions[i] === end) {
      end++
    } else {
      ranges.push([start, end])
      start = positions[i]
      end = positions[i] + 1
    }
  }
  ranges.push([start, end])
  return ranges
}
