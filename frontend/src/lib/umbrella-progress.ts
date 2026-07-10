import type { Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'

export interface UmbrellaProgress {
  done: number
  total: number
}

export interface UmbrellaChildren {
  children: Task[]
  unresolved: string[]
  displayTotal: number
}

const LANDED_OUTCOMES = new Set(['merged', 'merged_with_edits'])

// "Shipped" in the UI means the child is locally done and its outcome records
// a landed merge shape, not merely that some PR existed or the issue closed.
export function isChildComplete(task: Task): boolean {
  return task.status === 'done' && LANDED_OUTCOMES.has(task.outcome ?? '')
}

export function buildUmbrellaProgress(tasks: Task[]): Map<string, UmbrellaProgress> {
  const byUmbrella = new Map<string, UmbrellaProgress>()
  for (const task of tasks) {
    const key = normalizeIssueRef(task.umbrellaIssue ?? '')
    if (!key) continue
    const progress = byUmbrella.get(key) ?? { done: 0, total: 0 }
    progress.total += 1
    if (isChildComplete(task)) progress.done += 1
    byUmbrella.set(key, progress)
  }
  return byUmbrella
}

// childrenForUmbrella resolves the display rows for a task's Children panel:
// materialized child tasks (linked via umbrellaIssue) ordered by the umbrella's
// declared dependsOn, plus unresolved dependsOn refs with no matching local task.
export function childrenForUmbrella(umbrella: Task, tasks: Task[]): UmbrellaChildren {
  const umbrellaKey = normalizeIssueRef(umbrella.issue ?? '')

  const byIssue = new Map<string, Task>()
  for (const task of tasks) {
    const key = normalizeIssueRef(task.issue ?? '')
    if (key) byIssue.set(key, task)
  }

  const materialized = umbrellaKey
    ? tasks.filter((task) => normalizeIssueRef(task.umbrellaIssue ?? '') === umbrellaKey)
    : []

  const declaredRefs = (umbrella.dependsOn ?? [])
    .map((ref) => normalizeIssueRef(ref))
    .filter((ref) => ref !== '')

  const orderIndex = new Map<string, number>()
  declaredRefs.forEach((ref, i) => {
    if (!orderIndex.has(ref)) orderIndex.set(ref, i)
  })

  const materializedByIssue = new Map<string, Task>()
  for (const task of materialized) {
    const key = normalizeIssueRef(task.issue ?? '')
    if (key) materializedByIssue.set(key, task)
  }

  const ordered: Task[] = []
  const unordered: Task[] = []
  for (const task of materialized) {
    const key = normalizeIssueRef(task.issue ?? '')
    if (key && orderIndex.has(key)) continue
    unordered.push(task)
  }
  const sortedRefs = [...orderIndex.entries()].sort((a, b) => a[1] - b[1])
  for (const [ref] of sortedRefs) {
    const task = materializedByIssue.get(ref)
    if (task) ordered.push(task)
  }
  const children = [...ordered, ...unordered]

  const unresolvedSet = new Set<string>()
  const unresolved: string[] = []
  for (const ref of declaredRefs) {
    if (byIssue.has(ref)) continue
    if (unresolvedSet.has(ref)) continue
    unresolvedSet.add(ref)
    unresolved.push(ref)
  }

  return { children, unresolved, displayTotal: children.length + unresolved.length }
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
