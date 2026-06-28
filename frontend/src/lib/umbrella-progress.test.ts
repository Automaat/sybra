import { describe, expect, it } from 'vitest'
import { Status, TaskType, type Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'
import {
  buildUmbrellaProgress,
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
      task({ id: 'c1', status: Status.StatusDone, umbrellaIssue: 'https://github.com/Automaat/sybra/issues/1213' }),
      task({ id: 'c2', status: Status.StatusInProgress, umbrellaIssue: 'Automaat/sybra#1213' }),
      task({ id: 'c3', status: Status.StatusDone, umbrellaIssue: 'Automaat/sybra#999' }),
      task({ id: 'standalone', status: Status.StatusDone }),
    ])

    expect(byUmbrella.get('automaat/sybra#1213')).toEqual({ done: 1, total: 2 })
    expect(byUmbrella.get('automaat/sybra#999')).toEqual({ done: 1, total: 1 })
  })

  it('resolves tracker progress from its issue ref', () => {
    const byUmbrella = buildUmbrellaProgress([
      task({ id: 'c1', status: Status.StatusDone, umbrellaIssue: 'Automaat/sybra#1213' }),
      task({ id: 'c2', status: Status.StatusTodo, umbrellaIssue: 'Automaat/sybra#1213' }),
    ])

    expect(progressForUmbrellaTracker(
      task({ taskType: TaskType.TaskTypeUmbrella, issue: 'https://github.com/Automaat/sybra/issues/1213' }),
      byUmbrella,
    )).toEqual({ done: 1, total: 2 })
    expect(progressForUmbrellaTracker(task({ taskType: TaskType.TaskTypeNormal }), byUmbrella)).toBeNull()
  })
})
