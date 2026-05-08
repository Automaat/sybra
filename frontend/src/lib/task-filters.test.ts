import { describe, it, expect } from 'vitest'
import {
  matchesQuery,
  matchesProject,
  matchesTags,
  matchesAgentMode,
  matchesDateRange,
} from './task-filters.js'
import type { Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'

function task(over: Partial<Task> = {}): Task {
  return {
    id: 't1',
    title: 'Hello world',
    body: 'task body text',
    status: 'todo',
    tags: ['backend'],
    agentMode: 'headless',
    projectId: 'owner/repo',
    issue: '',
    closedAt: null,
    updatedAt: '2026-04-01T00:00:00Z',
    ...over,
  } as unknown as Task
}

describe('matchesQuery', () => {
  it('passes empty query', () => {
    expect(matchesQuery(task(), '')).toBe(true)
    expect(matchesQuery(task(), '   ')).toBe(true)
  })
  it('matches title case-insensitively', () => {
    expect(matchesQuery(task({ title: 'My Task' }), 'task')).toBe(true)
    expect(matchesQuery(task({ title: 'My Task' }), 'TASK')).toBe(true)
  })
  it('matches body', () => {
    expect(matchesQuery(task({ body: 'auth middleware' }), 'middleware')).toBe(true)
  })
  it('matches issue url', () => {
    expect(matchesQuery(task({ issue: 'https://github.com/foo/bar/issues/1' }), 'foo/bar')).toBe(
      true,
    )
  })
  it('rejects when no field matches', () => {
    expect(matchesQuery(task({ title: 'a', body: 'b' }), 'zzz')).toBe(false)
  })
})

describe('matchesProject', () => {
  it('passes empty', () => {
    expect(matchesProject(task(), '')).toBe(true)
  })
  it('matches projectId', () => {
    expect(matchesProject(task({ projectId: 'foo/bar' }), 'foo/bar')).toBe(true)
  })
  it('rejects when projectId differs', () => {
    expect(matchesProject(task({ projectId: 'foo/bar' }), 'baz/qux')).toBe(false)
  })
})

describe('matchesTags', () => {
  it('passes empty list', () => {
    expect(matchesTags(task(), [])).toBe(true)
  })
  it('requires every tag to be present', () => {
    expect(matchesTags(task({ tags: ['a', 'b', 'c'] }), ['a', 'b'])).toBe(true)
    expect(matchesTags(task({ tags: ['a'] }), ['a', 'b'])).toBe(false)
  })
  it('rejects when task tags missing', () => {
    expect(matchesTags(task({ tags: undefined as unknown as string[] }), ['a'])).toBe(false)
  })
})

describe('matchesAgentMode', () => {
  it('passes empty', () => {
    expect(matchesAgentMode(task(), '')).toBe(true)
  })
  it('matches and rejects', () => {
    expect(matchesAgentMode(task({ agentMode: 'headless' }), 'headless')).toBe(true)
    expect(matchesAgentMode(task({ agentMode: 'interactive' }), 'headless')).toBe(false)
  })
})

describe('matchesDateRange', () => {
  const t1 = task({ closedAt: '2026-04-15T12:00:00Z' as unknown as Task['closedAt'] })

  it('passes when both bounds are null', () => {
    expect(matchesDateRange(t1, null, null, 'closedAt')).toBe(true)
  })
  it('rejects when field is missing', () => {
    expect(matchesDateRange(task({ closedAt: null }), new Date('2026-01-01'), null, 'closedAt')).toBe(false)
  })
  it('respects from boundary', () => {
    expect(matchesDateRange(t1, new Date('2026-04-01'), null, 'closedAt')).toBe(true)
    expect(matchesDateRange(t1, new Date('2026-05-01'), null, 'closedAt')).toBe(false)
  })
  it('respects to boundary', () => {
    expect(matchesDateRange(t1, null, new Date('2026-05-01'), 'closedAt')).toBe(true)
    expect(matchesDateRange(t1, null, new Date('2026-04-01'), 'closedAt')).toBe(false)
  })
})
