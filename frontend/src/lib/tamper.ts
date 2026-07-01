import type { Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'

export const TAMPER_FLAGGED_REASON_PREFIX = 'possible test tampering — needs human bless before review:'

export function isTamperFlaggedTask(task: Task): boolean {
  return task.status === 'human-required' && (task.statusReason ?? '').startsWith(TAMPER_FLAGGED_REASON_PREFIX)
}
