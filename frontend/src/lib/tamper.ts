import type { Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'

// Backend-computed — see task.Task.TamperFlagged (internal/task/model.go). Kept
// as a helper (rather than reading task.tamperFlagged inline everywhere) so the
// single call site can absorb a future rename without touching every component.
export function isTamperFlaggedTask(task: Task): boolean {
  return task.tamperFlagged
}
