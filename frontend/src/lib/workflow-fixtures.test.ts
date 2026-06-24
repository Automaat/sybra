import { describe, it, expect, afterEach } from 'vitest'
import { isFixtureWorkflow, showFixtures } from './workflow-fixtures.js'

describe('isFixtureWorkflow', () => {
  it('flags the e2e editor fixture by id and name', () => {
    expect(isFixtureWorkflow({ id: 'wf-editor-e2e', name: 'E2E Editor Fixture' })).toBe(true)
  })

  it('flags e2e-prefixed ids and fixture names', () => {
    expect(isFixtureWorkflow({ id: 'e2e-smoke' })).toBe(true)
    expect(isFixtureWorkflow({ name: 'My Fixture' })).toBe(true)
  })

  it('does not flag real workflows', () => {
    expect(isFixtureWorkflow({ id: 'pr-review', name: 'PR Review' })).toBe(false)
    expect(isFixtureWorkflow({ id: 'simple-task-implement', name: 'Simple Task — Implement' })).toBe(false)
    // "end-to-end" is not the "e2e" token.
    expect(isFixtureWorkflow({ id: 'end-to-end-deploy', name: 'End to End Deploy' })).toBe(false)
  })
})

describe('showFixtures', () => {
  afterEach(() => localStorage.clear())

  it('is false by default', () => {
    expect(showFixtures()).toBe(false)
  })

  it('is true when the reveal flag is set', () => {
    localStorage.setItem('sybra.showFixtures', 'true')
    expect(showFixtures()).toBe(true)
  })
})
