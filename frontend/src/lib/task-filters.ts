// Pure filter predicates for Task lists. Used by TaskList and Logbook.
import type { Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'

export function matchesQuery(task: Task, query: string): boolean {
  const q = query.toLowerCase().trim()
  if (!q) return true
  if (task.title?.toLowerCase().includes(q)) return true
  if ((task.body ?? '').toLowerCase().includes(q)) return true
  if ((task.issue ?? '').toLowerCase().includes(q)) return true
  return false
}

export function matchesProject(task: Task, projectId: string): boolean {
  if (!projectId) return true
  return task.projectId === projectId
}

export function matchesTags(task: Task, tags: string[]): boolean {
  if (!tags || tags.length === 0) return true
  const taskTags = task.tags ?? []
  return tags.every((tag) => taskTags.includes(tag))
}

export function matchesAgentMode(task: Task, mode: string): boolean {
  if (!mode) return true
  return task.agentMode === mode
}

export type DateField = 'closedAt' | 'updatedAt' | 'createdAt' | 'dueDate'

export function matchesDateRange(
  task: Task,
  from: Date | null,
  to: Date | null,
  field: DateField,
): boolean {
  if (!from && !to) return true
  const raw = (task as unknown as Record<string, unknown>)[field]
  if (!raw) return false
  const d = new Date(raw as string | number | Date)
  if (isNaN(d.getTime())) return false
  if (from && d < from) return false
  if (to && d > to) return false
  return true
}
