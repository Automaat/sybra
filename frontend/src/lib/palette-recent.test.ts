import { describe, it, expect, beforeEach } from 'vitest'
import { getRecent, pushRecent } from './palette-recent.js'

const KEY = 'sybra.palette.recent'

beforeEach(() => {
  localStorage.clear()
})

describe('getRecent', () => {
  it('returns empty array when storage is empty', () => {
    expect(getRecent()).toEqual([])
  })

  it('returns empty array when storage is malformed JSON', () => {
    localStorage.setItem(KEY, 'not-json{{')
    expect(getRecent()).toEqual([])
  })

  it('returns empty array when stored value is not an array', () => {
    localStorage.setItem(KEY, '{"a":1}')
    expect(getRecent()).toEqual([])
  })

  it('filters out non-string entries', () => {
    localStorage.setItem(KEY, '["a", 1, null, "b"]')
    expect(getRecent()).toEqual(['a', 'b'])
  })

  it('returns stored ids in order', () => {
    localStorage.setItem(KEY, '["id1", "id2", "id3"]')
    expect(getRecent()).toEqual(['id1', 'id2', 'id3'])
  })
})

describe('pushRecent', () => {
  it('adds id to front', () => {
    pushRecent('a')
    expect(getRecent()).toEqual(['a'])
  })

  it('prepends new id and deduplicates existing', () => {
    localStorage.setItem(KEY, '["a", "b", "c"]')
    pushRecent('d')
    expect(getRecent()).toEqual(['d', 'a', 'b', 'c'])
  })

  it('moves existing id to front (no duplicate)', () => {
    localStorage.setItem(KEY, '["a", "b", "c"]')
    pushRecent('b')
    expect(getRecent()).toEqual(['b', 'a', 'c'])
  })

  it('caps at 5 entries (FIFO drop)', () => {
    localStorage.setItem(KEY, '["a", "b", "c", "d", "e"]')
    pushRecent('f')
    expect(getRecent()).toEqual(['f', 'a', 'b', 'c', 'd'])
  })

  it('moving existing id to front does not exceed cap', () => {
    localStorage.setItem(KEY, '["a", "b", "c", "d", "e"]')
    pushRecent('e')
    expect(getRecent()).toHaveLength(5)
    expect(getRecent()[0]).toBe('e')
  })
})
