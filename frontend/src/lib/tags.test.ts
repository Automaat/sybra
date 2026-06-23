import { describe, it, expect } from 'vitest'
import { normalizeTag, canonicalTags, matchesCanonicalTags } from './tags.js'

describe('normalizeTag', () => {
  it('strips known grouping prefixes', () => {
    expect(normalizeTag('kind/bug')).toBe('bug')
    expect(normalizeTag('type/refactor')).toBe('refactor')
    expect(normalizeTag('size/medium')).toBe('medium')
    expect(normalizeTag('priority/normal')).toBe('normal')
  })

  it('collapses synonyms', () => {
    expect(normalizeTag('performance')).toBe('perf')
    expect(normalizeTag('testing')).toBe('test')
    expect(normalizeTag('type/test')).toBe('test')
    expect(normalizeTag('enhancement')).toBe('feature')
  })

  it('lowercases and trims', () => {
    expect(normalizeTag('  Bug ')).toBe('bug')
  })

  it('leaves unknown prefixes intact', () => {
    expect(normalizeTag('frontend/login')).toBe('frontend/login')
  })

  it('does not strip location namespaces (area/scope) — they carry meaning', () => {
    // area/test must NOT collapse to the same canonical as type/test.
    expect(normalizeTag('area/test')).toBe('area/test')
    expect(normalizeTag('type/test')).toBe('test')
    expect(normalizeTag('area/test')).not.toBe(normalizeTag('type/test'))
  })

  it('reduces stray-slash tags to empty', () => {
    expect(normalizeTag('/')).toBe('')
    expect(normalizeTag('kind/')).toBe('')
  })

  it('passes unknown bare tags through', () => {
    expect(normalizeTag('backend')).toBe('backend')
  })
})

describe('canonicalTags', () => {
  it('dedupes variants to one sorted set', () => {
    expect(canonicalTags(['bug', 'kind/bug', 'feature', 'enhancement', 'perf', 'performance'])).toEqual([
      'bug',
      'feature',
      'perf',
    ])
  })

  it('drops empties', () => {
    expect(canonicalTags(['', '  ', 'bug'])).toEqual(['bug'])
  })
})

describe('matchesCanonicalTags', () => {
  it('matches a canonical selection against any variant', () => {
    expect(matchesCanonicalTags(['kind/bug'], ['bug'])).toBe(true)
    expect(matchesCanonicalTags(['bug'], ['bug'])).toBe(true)
    expect(matchesCanonicalTags(['feature'], ['bug'])).toBe(false)
  })

  it('requires every selected tag (AND)', () => {
    expect(matchesCanonicalTags(['bug', 'perf'], ['bug', 'perf'])).toBe(true)
    expect(matchesCanonicalTags(['bug'], ['bug', 'perf'])).toBe(false)
  })

  it('empty selection matches everything', () => {
    expect(matchesCanonicalTags(['bug'], [])).toBe(true)
    expect(matchesCanonicalTags(undefined, [])).toBe(true)
  })
})
