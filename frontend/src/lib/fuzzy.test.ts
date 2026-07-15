import { describe, it, expect } from 'vitest'
import { score, matchRanges } from './fuzzy.js'

describe('score', () => {
  it('returns 0 for empty query', () => {
    expect(score('', 'anything')).toBe(0)
  })

  it('returns null for empty target with non-empty query', () => {
    expect(score('a', '')).toBeNull()
  })

  it('returns null when query chars not in target', () => {
    expect(score('xyz', 'abc')).toBeNull()
  })

  it('matches case-insensitively', () => {
    expect(score('abc', 'ABC')).not.toBeNull()
    expect(score('ABC', 'abc')).not.toBeNull()
  })

  it('exact match scores higher than spread subsequence', () => {
    // 'cmd' as exact string vs 'cmd' scattered across 'c_m_d'
    const exact = score('cmd', 'cmd')
    const subseq = score('cmd', 'c_m_d')
    expect(exact).not.toBeNull()
    expect(subseq).not.toBeNull()
    expect(exact!).toBeGreaterThan(subseq!)
  })

  it('consecutive match bonus: chars found consecutively score higher than spread', () => {
    // 'cmd' consecutive from start of 'cmds' vs spread in 'c-m-d'
    const consecutive = score('cmd', 'cmds')
    const spread = score('cmd', 'c-m-d')
    expect(consecutive).not.toBeNull()
    expect(spread).not.toBeNull()
    expect(consecutive!).toBeGreaterThan(spread!)
  })

  it('word boundary bonus: match at start scores higher than mid-word', () => {
    const atStart = score('set', 'Settings')
    const midWord = score('set', 'reset')
    expect(atStart).not.toBeNull()
    expect(midWord).not.toBeNull()
    expect(atStart!).toBeGreaterThan(midWord!)
  })

  it('word boundary bonus applies after hyphen/underscore/slash/space', () => {
    const afterHyphen = score('c', 'a-c')
    const afterSlash = score('c', 'a/c')
    const afterUnderscore = score('c', 'a_c')
    const afterSpace = score('c', 'a c')
    const midWord = score('c', 'ac')
    expect(afterHyphen!).toBeGreaterThan(midWord!)
    expect(afterSlash!).toBeGreaterThan(midWord!)
    expect(afterUnderscore!).toBeGreaterThan(midWord!)
    expect(afterSpace!).toBeGreaterThan(midWord!)
  })

  it('shorter gap penalty: few gaps scores higher than many gaps', () => {
    const noGap = score('ab', 'ab')
    const bigGap = score('ab', 'a___b')
    expect(noGap!).toBeGreaterThan(bigGap!)
  })

  it('no spurious consecutive bonus on the first matched character', () => {
    // First char always gets base + (optional boundary), never consecutive
    const first = score('a', 'axxx')  // a at pos 0, word boundary
    const boundary = score('a', 'x-a') // a at pos 2, word boundary
    // Both get word boundary; first char shouldn't inflate via consecutive
    expect(first).toBe(20) // 10 base + 10 boundary
    expect(boundary).toBe(20) // 10 base + 10 boundary
  })

  it('returns null if target is too short to contain all query chars', () => {
    expect(score('abcde', 'abc')).toBeNull()
  })

  it('full match on short query', () => {
    expect(score('s', 'settings')).not.toBeNull()
    expect(score('s', 'dashboard')).not.toBeNull()
  })
})

describe('matchRanges', () => {
  it('returns empty array for empty query', () => {
    expect(matchRanges('', 'abc')).toEqual([])
  })

  it('returns null if no match', () => {
    expect(matchRanges('xyz', 'abc')).toBeNull()
  })

  it('merges consecutive positions into a single range', () => {
    expect(matchRanges('abc', 'abcdef')).toEqual([[0, 3]])
  })

  it('separates non-consecutive positions into multiple ranges', () => {
    expect(matchRanges('ac', 'abc')).toEqual([[0, 1], [2, 3]])
  })

  it('is case-insensitive', () => {
    expect(matchRanges('abc', 'ABC')).toEqual([[0, 3]])
  })
})
