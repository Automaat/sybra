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

  it('counts only locally merged-outcome children, not false completions', () => {
    const byUmbrella = buildUmbrellaProgress([
      task({ id: 'c1', status: Status.StatusDone, outcome: 'merged', umbrellaIssue: 'https://github.com/Automaat/sybra/issues/1213' }),
      task({ id: 'c5', status: Status.StatusInProgress, outcome: 'merged_later', umbrellaIssue: 'https://github.com/Automaat/sybra/issues/1213' }),
      task({ id: 'c2', status: Status.StatusInProgress, umbrellaIssue: 'Automaat/sybra#1213' }),
      task({ id: 'c4', status: Status.StatusDone, outcome: '', umbrellaIssue: 'Automaat/sybra#1213' }),
      task({ id: 'c3', status: Status.StatusDone, outcome: 'merged_with_edits', umbrellaIssue: 'Automaat/sybra#999' }),
      task({ id: 'standalone', status: Status.StatusDone, outcome: 'merged' }),
    ])

    expect(byUmbrella.get('automaat/sybra#1213')).toEqual({ done: 2, total: 4 })
    expect(byUmbrella.get('automaat/sybra#999')).toEqual({ done: 1, total: 1 })
  })

  it('resolves tracker progress from its issue ref', () => {
    const byUmbrella = buildUmbrellaProgress([
      task({ id: 'c1', status: Status.StatusDone, outcome: 'merged', umbrellaIssue: 'Automaat/sybra#1213' }),
      task({ id: 'c2', status: Status.StatusTodo, umbrellaIssue: 'Automaat/sybra#1213' }),
    ])

    expect(progressForUmbrellaTracker(
      task({ taskType: TaskType.TaskTypeUmbrella, issue: 'https://github.com/Automaat/sybra/issues/1213' }),
      byUmbrella,
    )).toEqual({ done: 1, total: 2 })
    expect(progressForUmbrellaTracker(task({ taskType: TaskType.TaskTypeNormal }), byUmbrella)).toBeNull()
  })

  describe('isChildComplete', () => {
    it('counts merged outcome prefixes as complete without relying on local status', () => {
      expect(isChildComplete(task({ outcome: 'merged' }))).toBe(true)
      expect(isChildComplete(task({ status: Status.StatusTodo, outcome: 'merged_with_edits' }))).toBe(true)
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
      })
      // Real umbrella trackers never carry their own dependsOn (see
      // internal/umbrella/expand.go's materialize()) -- declared order and
      // unresolved refs must come from the materialized children's own
      // dependsOn instead.
      const c11 = task({
        id: 'c11',
        issue: 'Automaat/sybra#11',
        umbrellaIssue: 'Automaat/sybra#1213',
        dependsOn: ['https://github.com/Automaat/sybra/issues/10'],
      })
      const c10 = task({
        id: 'c10',
        issue: 'Automaat/sybra#10',
        umbrellaIssue: 'Automaat/sybra#1213',
        dependsOn: ['https://github.com/Automaat/sybra/issues/99'],
      })
      const unordered = task({ id: 'cX', issue: 'Automaat/sybra#77', umbrellaIssue: 'Automaat/sybra#1213' })
      // Materialized list is intentionally out of dependsOn order to prove the resolver re-sorts.
      const tasks = [c11, c10, unordered]

      const result = childrenForUmbrella(umbrella, tasks)

      expect(result.children.map((t) => t.id)).toEqual(['c10', 'c11', 'cX'])
      expect(result.unresolved).toEqual(['automaat/sybra#99'])
      expect(result.displayTotal).toBe(4)
    })

    it('surfaces a declared-but-unmaterialized child dropped from a real umbrella DAG', () => {
      // Regression for the production shape: the umbrella tracker itself
      // never has dependsOn populated. A sibling child (c2) declares a
      // dependency on an issue (#102) that was planned but never
      // materialized as a local task -- e.g. dropped by a race between the
      // planner and materialize(). That must surface as unresolved, not be
      // silently swallowed because it was never read off the umbrella task.
      const umbrella = task({
        id: 'u1',
        taskType: TaskType.TaskTypeUmbrella,
        issue: 'o/r#100',
        dependsOn: [],
      })
      const c1 = task({ id: 'c1', issue: 'o/r#101', umbrellaIssue: 'o/r#100', dependsOn: [] })
      const c2 = task({ id: 'c2', issue: 'o/r#103', umbrellaIssue: 'o/r#100', dependsOn: ['o/r#102'] })

      const result = childrenForUmbrella(umbrella, [c1, c2])

      expect(result.children.map((t) => t.id)).toEqual(['c1', 'c2'])
      expect(result.unresolved).toEqual(['o/r#102'])
      expect(result.displayTotal).toBe(3)
    })

    it('keeps a child-declared ref unresolved when a same-issue task exists outside the umbrella', () => {
      const umbrella = task({
        id: 'u1',
        taskType: TaskType.TaskTypeUmbrella,
        issue: 'o/r#100',
      })
      const materializedChild = task({
        id: 'c1',
        issue: 'o/r#101',
        umbrellaIssue: 'o/r#100',
        dependsOn: ['o/r#102'],
      })
      const mismatchedLocalTask = task({
        id: 'c2',
        issue: 'o/r#102',
        umbrellaIssue: 'o/r#999',
      })

      const result = childrenForUmbrella(umbrella, [materializedChild, mismatchedLocalTask])

      expect(result.children.map((t) => t.id)).toEqual(['c1'])
      expect(result.unresolved).toEqual(['o/r#102'])
      expect(result.displayTotal).toBe(2)
    })

    it('resolves a tracker-declared dependsOn ref to a local child by issue', () => {
      const umbrella = task({
        id: 'manual-umbrella',
        taskType: TaskType.TaskTypeUmbrella,
        issue: 'https://github.com/Automaat/sybra/issues/100',
        dependsOn: ['https://github.com/Automaat/sybra/issues/101'],
      })
      const localChild = task({
        id: 'manual-child',
        title: 'Manual child by dependsOn only',
        taskType: TaskType.TaskTypeNormal,
        issue: 'https://github.com/Automaat/sybra/issues/101',
        umbrellaIssue: '',
        outcome: 'merged',
      })

      const result = childrenForUmbrella(umbrella, [localChild])

      expect(result.children.map((t) => t.id)).toEqual(['manual-child'])
      expect(result.unresolved).toEqual([])
      expect(result.displayTotal).toBe(1)
    })

    it('excludes unresolved refs from the children list and dedupes repeats', () => {
      const umbrella = task({
        id: 'u1',
        taskType: TaskType.TaskTypeUmbrella,
        issue: 'Automaat/sybra#1',
      })
      const child = task({
        id: 'c1',
        issue: 'Automaat/sybra#3',
        umbrellaIssue: 'Automaat/sybra#1',
        dependsOn: ['Automaat/sybra#2', 'Automaat/sybra#2', 'https://github.com/Automaat/sybra/issues/2'],
      })

      const result = childrenForUmbrella(umbrella, [child])

      expect(result.children.map((t) => t.id)).toEqual(['c1'])
      expect(result.unresolved).toEqual(['automaat/sybra#2'])
      expect(result.displayTotal).toBe(2)
    })

    it('renders a normal task with dependsOn as a Children panel of its own prerequisites', () => {
      const childTask = task({
        id: 'child',
        taskType: TaskType.TaskTypeNormal,
        issue: 'Automaat/sybra#10',
        dependsOn: ['Automaat/sybra#1', 'Automaat/sybra#2'],
      })

      const result = childrenForUmbrella(childTask, [
        task({ id: 'prereq1', issue: 'Automaat/sybra#1' }),
      ])

      expect(result.children.map((t) => t.id)).toEqual(['prereq1'])
      expect(result.unresolved).toEqual(['automaat/sybra#2'])
      expect(result.displayTotal).toBe(2)
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
