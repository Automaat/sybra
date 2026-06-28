import type { Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'

export interface UmbrellaProgress {
  done: number
  total: number
}

export function buildUmbrellaProgress(tasks: Task[]): Map<string, UmbrellaProgress> {
  const byUmbrella = new Map<string, UmbrellaProgress>()
  for (const task of tasks) {
    const key = normalizeIssueRef(task.umbrellaIssue ?? '')
    if (!key) continue
    const progress = byUmbrella.get(key) ?? { done: 0, total: 0 }
    progress.total += 1
    if (task.status === 'done') progress.done += 1
    byUmbrella.set(key, progress)
  }
  return byUmbrella
}

export function progressForUmbrellaTracker(
  task: Task,
  byUmbrella: Map<string, UmbrellaProgress>,
): UmbrellaProgress | null {
  if (task.taskType !== 'umbrella') return null
  const key = normalizeIssueRef(task.issue ?? '')
  if (!key) return null
  return byUmbrella.get(key) ?? { done: 0, total: 0 }
}

export function normalizeIssueRef(ref: string): string {
  const trimmed = ref.trim()
  if (!trimmed) return ''
  const github = trimmed.match(/^https?:\/\/(?:www\.)?github\.com\/([^/]+)\/([^/]+)\/(?:issues|pull)\/(\d+)/i)
  if (github) {
    return `${github[1].toLowerCase()}/${github[2].toLowerCase()}#${github[3]}`
  }
  return trimmed.toLowerCase()
}
