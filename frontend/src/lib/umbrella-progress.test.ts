import { describe, expect, it } from 'vitest'
import { Status, TaskType, type Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'
import {
  buildUmbrellaProgress,
  childrenForUmbrella,
  isChildComplete,
  normalizeIssueRef,
  progressForUmbrellaTracker,
} from './umbrella-progress.js'

function task(overrides: Partial<Task>): Task {
  return {
    id: '',
    title: '',
    status: Status.StatusTodo,
    tags: [],
    agentMode: 'headless',
    issue: '',
    umbrellaIssue: '',
    taskType: TaskType.$zero,
    updatedAt: '',
    createdAt: '',
    body: '',
    ...overrides,
  } as Task
}

describe('umbrella progress', () => {
  it('normalizes GitHub issue refs across URL and shorthand forms', () => {
    expect(normalizeIssueRef('https://github.com/Automaat/sybra/issues/1213')).toBe('automaat/sybra#1213')
    expect(normalizeIssueRef('Automaat/sybra#1213')).toBe('automaat/sybra#1213')
  })

  it('counts done and total materialized children per umbrella', () => {
    const byUmbrella = buildUmbrellaProgress([
      task({ id: 'c1', outcome: 'merged', umbrellaIssue: 'https://github.com/Automaat/sybra/issues/1213' }),
      task({ id: 'c2', status: Status.StatusInProgress, umbrellaIssue: 'Automaat/sybra#1213' }),
      task({ id: 'c3', outcome: 'merged', umbrellaIssue: 'Automaat/sybra#999' }),
      task({ id: 'standalone', status: Status.StatusDone }),
    ])

    expect(byUmbrella.get('automaat/sybra#1213')).toEqual({ done: 1, total: 2 })
    expect(byUmbrella.get('automaat/sybra#999')).toEqual({ done: 1, total: 1 })
  })

  it('resolves tracker progress from its issue ref', () => {
    const byUmbrella = buildUmbrellaProgress([
      task({ id: 'c1', outcome: 'merged', umbrellaIssue: 'Automaat/sybra#1213' }),
      task({ id: 'c2', status: Status.StatusTodo, umbrellaIssue: 'Automaat/sybra#1213' }),
    ])

    expect(progressForUmbrellaTracker(
      task({ taskType: TaskType.TaskTypeUmbrella, issue: 'https://github.com/Automaat/sybra/issues/1213' }),
      byUmbrella,
    )).toEqual({ done: 1, total: 2 })
    expect(progressForUmbrellaTracker(task({ taskType: TaskType.TaskTypeNormal }), byUmbrella)).toBeNull()
  })

  describe('isChildComplete', () => {
    it('counts merged and merged_with_edits outcomes as complete', () => {
      expect(isChildComplete(task({ outcome: 'merged' }))).toBe(true)
      expect(isChildComplete(task({ outcome: 'merged_with_edits' }))).toBe(true)
    })

    it('does not count closed, reverted, empty, or bare local done', () => {
      expect(isChildComplete(task({ outcome: 'closed' }))).toBe(false)
      expect(isChildComplete(task({ outcome: 'reverted' }))).toBe(false)
      expect(isChildComplete(task({ outcome: '' }))).toBe(false)
      expect(isChildComplete(task({ status: Status.StatusDone, outcome: '' }))).toBe(false)
      expect(isChildComplete(task({ status: Status.StatusDone, prNumber: 0 }))).toBe(false)
    })
  })

  describe('childrenForUmbrella', () => {
    it('resolves materialized children ordered by dependsOn, unresolved refs, and displayTotal', () => {
      const umbrella = task({
        id: 'u1',
        taskType: TaskType.TaskTypeUmbrella,
        issue: 'https://github.com/Automaat/sybra/issues/1213',
        dependsOn: [
          'https://github.com/Automaat/sybra/issues/10',
          'https://github.com/Automaat/sybra/issues/11',
          'https://github.com/Automaat/sybra/issues/99',
        ],
      })
      const c11 = task({ id: 'c11', issue: 'Automaat/sybra#11', umbrellaIssue: 'Automaat/sybra#1213' })
      const c10 = task({ id: 'c10', issue: 'Automaat/sybra#10', umbrellaIssue: 'Automaat/sybra#1213' })
      const unordered = task({ id: 'cX', issue: 'Automaat/sybra#77', umbrellaIssue: 'Automaat/sybra#1213' })
      // Materialized list is intentionally out of dependsOn order to prove the resolver re-sorts.
      const tasks = [c11, c10, unordered]

      const result = childrenForUmbrella(umbrella, tasks)

      expect(result.children.map((t) => t.id)).toEqual(['c10', 'c11', 'cX'])
      expect(result.unresolved).toEqual(['automaat/sybra#99'])
      expect(result.displayTotal).toBe(4)
    })

    it('excludes unresolved refs from the children list and dedupes repeats', () => {
      const umbrella = task({
        id: 'u1',
        issue: 'Automaat/sybra#1',
        dependsOn: ['Automaat/sybra#2', 'Automaat/sybra#2', 'https://github.com/Automaat/sybra/issues/2'],
      })

      const result = childrenForUmbrella(umbrella, [])

      expect(result.children).toEqual([])
      expect(result.unresolved).toEqual(['automaat/sybra#2'])
      expect(result.displayTotal).toBe(1)
    })

    it('returns no rows for a task with neither umbrella children nor dependsOn', () => {
      const result = childrenForUmbrella(task({ id: 'solo', issue: 'Automaat/sybra#5' }), [
        task({ id: 'other', issue: 'Automaat/sybra#6' }),
      ])

      expect(result.children).toEqual([])
      expect(result.unresolved).toEqual([])
      expect(result.displayTotal).toBe(0)
    })
  })
})
