import { describe, it, expect } from 'vitest'
import { projectShortName, projectDotStyle } from './project-cue.js'

describe('projectShortName', () => {
  it('drops the owner prefix', () => {
    expect(projectShortName('Automaat/home-dashboard')).toBe('home-dashboard')
  })

  it('returns the id unchanged when there is no owner', () => {
    expect(projectShortName('home-dashboard')).toBe('home-dashboard')
  })

  it('keeps only the last segment for nested paths', () => {
    expect(projectShortName('org/group/repo')).toBe('repo')
  })
})

describe('projectDotStyle', () => {
  it('is deterministic for a given project', () => {
    expect(projectDotStyle('Automaat/a')).toBe(projectDotStyle('Automaat/a'))
  })

  it('differs between projects', () => {
    expect(projectDotStyle('Automaat/a')).not.toBe(projectDotStyle('Automaat/b'))
  })

  it('produces a restrained OKLCH colour (fixed lightness/chroma)', () => {
    expect(projectDotStyle('x')).toMatch(/^background-color: oklch\(68% 0\.11 \d{1,3}deg\)$/)
  })
})
