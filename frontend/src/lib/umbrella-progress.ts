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

// Local shipped-work proxy: outcome is stamped by Sybra's PR monitor when a
// task's own PR lands. `closed` means closed-unmerged, not completed work.
export function isChildComplete(task: Task): boolean {
  return (task.outcome ?? '').startsWith('merged')
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

// childrenForUmbrella resolves the display rows for a task's Children panel.
//
// Two shapes feed this panel:
//  - An umbrella tracker (taskType === 'umbrella'): children are materialized
//    tasks linked via umbrellaIssue. The tracker itself never carries its own
//    dependsOn (internal/umbrella/expand.go's materialize() only sets
//    DependsOn on the per-child CreateFull call, never on the tracker's), so
//    declared refs — used both for ordering and for surfacing declared-but
//    -unmaterialized children — are aggregated from the materialized
//    children's own dependsOn instead. The union also includes the tracker's
//    dependsOn (harmless, and covers callers that do populate it directly).
//  - Any other task with a non-empty dependsOn (its own prerequisites):
//    children are that task's declared refs resolved against the local task
//    list by issue, in declared order.
export function childrenForUmbrella(task: Task, tasks: Task[]): UmbrellaChildren {
  const isUmbrella = task.taskType === 'umbrella'
  if (!isUmbrella && (task.dependsOn ?? []).length === 0) {
    return { children: [], unresolved: [], displayTotal: 0 }
  }

  const byIssue = new Map<string, Task>()
  for (const t of tasks) {
    const key = normalizeIssueRef(t.issue ?? '')
    if (key) byIssue.set(key, t)
  }

  const taskKey = normalizeIssueRef(task.issue ?? '')
  const materialized = isUmbrella && taskKey
    ? tasks.filter((t) => normalizeIssueRef(t.umbrellaIssue ?? '') === taskKey)
    : []

  const declaredRefs: string[] = []
  const seenDeclared = new Set<string>()
  const rawRefLists = isUmbrella
    ? [...materialized.map((t) => t.dependsOn ?? []), task.dependsOn ?? []]
    : [task.dependsOn ?? []]
  for (const list of rawRefLists) {
    for (const ref of list) {
      const key = normalizeIssueRef(ref)
      if (!key || seenDeclared.has(key)) continue
      seenDeclared.add(key)
      declaredRefs.push(key)
    }
  }

  const orderIndex = new Map<string, number>()
  declaredRefs.forEach((ref, i) => {
    if (!orderIndex.has(ref)) orderIndex.set(ref, i)
  })

  let children: Task[]
  if (isUmbrella) {
    const materializedByIssue = new Map<string, Task>()
    for (const t of materialized) {
      const key = normalizeIssueRef(t.issue ?? '')
      if (key) materializedByIssue.set(key, t)
    }

    const ordered: Task[] = []
    const unordered: Task[] = []
    for (const t of materialized) {
      const key = normalizeIssueRef(t.issue ?? '')
      if (key && orderIndex.has(key)) continue
      unordered.push(t)
    }
    const sortedRefs = [...orderIndex.entries()].sort((a, b) => a[1] - b[1])
    for (const [ref] of sortedRefs) {
      const t = materializedByIssue.get(ref)
      if (t) ordered.push(t)
    }
    children = [...ordered, ...unordered]
  } else {
    children = declaredRefs
      .map((ref) => byIssue.get(ref))
      .filter((t): t is Task => !!t)
  }

  const unresolved: string[] = []
  for (const ref of declaredRefs) {
    if (byIssue.has(ref)) continue
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
