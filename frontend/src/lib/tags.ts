// Tag taxonomy normalization for the logbook.
//
// Tasks accumulate tags from several sources (manual, GitHub issue labels,
// auto-sources), which produces heavy duplication: a grouping prefix
// (`kind/bug`, `type/refactor`, `size/medium`, `priority/normal`) alongside the
// bare term, plus synonyms (`performance`/`perf`, `testing`/`test`).
//
// `normalizeTag` collapses every variant to ONE canonical term so the logbook
// shows a small, de-duplicated tag set. The canonical convention is the bare,
// lowercase term (no grouping prefix); the documented vocabulary below is the
// target set.

// Grouping prefixes stripped from `<prefix>/<term>` tags. Deliberately excludes
// location namespaces like `area/`/`scope/` — those carry meaning (an
// `area/test` code-area is not a `type/test`), so stripping them would merge
// semantically distinct tags.
const GROUP_PREFIXES = new Set(['kind', 'type', 'size', 'priority'])

/** Synonyms that don't reduce to a simple prefix strip. */
const SYNONYMS: Record<string, string> = {
  performance: 'perf',
  testing: 'test',
  tests: 'test',
  enhancement: 'feature',
  feat: 'feature',
  docs: 'documentation',
  doc: 'documentation',
}

/**
 * Canonical, documented vocabulary the logbook normalizes toward. Not
 * exhaustive (unknown tags pass through normalized), but records the intended
 * de-duplicated set.
 */
export const CANONICAL_TAGS = [
  'bug',
  'feature',
  'refactor',
  'perf',
  'test',
  'documentation',
  'chore',
  'small',
  'medium',
  'large',
  'low',
  'normal',
  'high',
] as const

/** Reduce a raw tag to its canonical form (empty for meaningless tags). */
export function normalizeTag(tag: string): string {
  const lower = tag.trim().toLowerCase()
  const slash = lower.indexOf('/')
  const stripped = slash > 0 && GROUP_PREFIXES.has(lower.slice(0, slash)) ? lower.slice(slash + 1) : lower
  // Drop stray slashes/whitespace (e.g. `/`, `kind/`) so they never become chips.
  const base = stripped.replace(/^\/+|\/+$/g, '').trim()
  return SYNONYMS[base] ?? base
}

/** Distinct, sorted canonical tags from a raw tag list. */
export function canonicalTags(tags: string[]): string[] {
  return [...new Set(tags.map(normalizeTag))].filter(Boolean).sort()
}

/**
 * True when a task's tags satisfy every selected canonical tag (a task matches
 * canonical `bug` whether tagged `bug`, `kind/bug`, etc.). Empty selection
 * matches everything.
 */
export function matchesCanonicalTags(taskTags: string[] | undefined, selected: string[]): boolean {
  if (selected.length === 0) return true
  const normalized = new Set((taskTags ?? []).map(normalizeTag))
  return selected.every((t) => normalized.has(t))
}
