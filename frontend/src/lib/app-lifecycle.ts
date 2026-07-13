// App-level lifecycle wiring: store loads/polling + event subscriptions.
// Returns a single cleanup function that tears down everything the start()
// call set up. Pure imperative — callers should invoke from inside untrack()
// so $state writes inside the loads don't re-run the parent effect (the
// browser would otherwise build the world ~60×/s on stores reacting to each
// other; see comment trail in commit history).

import { EventsOn, GetProviderHealth, ProviderHealthEnabled } from '$lib/api'
import * as ev from './events.js'
import { taskStore } from '../stores/tasks.svelte.js'
import { agentStore } from '../stores/agents.svelte.js'
import { projectStore } from '../stores/projects.svelte.js'
import { notificationStore } from '../stores/notifications.svelte.js'
import { bgopStore } from '../stores/bgops.svelte.js'
import { connectionStore } from '../stores/connection.svelte.js'
import { reviewStore } from '../stores/reviews.svelte.js'
import { clusterStore } from '../stores/cluster.svelte.js'
import { navStore } from './navigation.svelte.js'

export type DegradedWarning = { subsystem: string; reason: string }

export type ProviderHealth = {
  provider: string
  healthy: boolean
  reason: string
  detail?: string
  lastCheck?: string
  ratelimitedUntil?: string
  failoverActive?: boolean
}

export interface AppLifecycleHooks {
  onDegraded: (w: DegradedWarning) => void
  onProviderHealthSnapshot: (snapshot: Record<string, ProviderHealth>) => void
  onProviderHealth: (p: ProviderHealth) => void
  onQuitConfirm: () => void
}

// The backend can fire dozens of task:created/updated/deleted events per second
// when agents churn (restart-stale loops, rapid workflow advances, large
// headless sessions). A full taskStore.load() on every event re-builds the
// reactive Map and forces every kanban card to re-render, which saturates the
// WebKit main thread and freezes the UI even though the Go side is idle.
//
// Instead we patch only the affected task. The watcher payload is the changed
// file's path: a task lives at `<id>.md`, its sidecars (plan, critique, drafts)
// at `<id>.<suffix>.md`. Task ids are dot-free (uuid[:8]), so the id is the
// basename's first segment, and a sidecar change maps to a patch of its parent.
function taskIdFromPath(path: string): string {
  const base = path.split('/').pop() ?? path
  return base.split('.')[0] ?? ''
}

// A primary task file is `<id>.md` exactly; a sidecar carries an extra dotted
// segment. Only removing the primary file deletes the task — a sidecar removal
// is a content change on the still-present parent.
function isPrimaryTaskFile(path: string): boolean {
  const base = path.split('/').pop() ?? path
  return /^[^.]+\.md$/.test(base)
}

type TaskEventKind = 'change' | 'delete'

// Coalesce a burst of file events into one action PER task id, flushed on a
// trailing timer. 50 events for one task collapse to a single fetch, and
// events for different tasks don't cancel each other. A `remove` is sticky: a
// task-file delete plus its sidecar-delete cascade must not downgrade back to a
// patch of an already-gone task.
function makeTaskEventCoalescer(): (path: string, kind: TaskEventKind) => void {
  const pending = new Map<string, 'patch' | 'remove'>()
  let timer: ReturnType<typeof setTimeout> | null = null
  const flushDelay = 120

  const flush = (): void => {
    timer = null
    const batch = [...pending.entries()]
    pending.clear()
    for (const [id, action] of batch) {
      if (action === 'remove') taskStore.removeOne(id)
      else void taskStore.patchOne(id)
    }
  }

  return (path: string, kind: TaskEventKind): void => {
    const id = taskIdFromPath(path)
    if (!id) return
    if (kind === 'delete' && isPrimaryTaskFile(path)) {
      pending.set(id, 'remove')
    } else if (pending.get(id) !== 'remove') {
      pending.set(id, 'patch')
    }
    if (timer === null) timer = setTimeout(flush, flushDelay)
  }
}

export function startAppLifecycle(hooks: AppLifecycleHooks): () => void {
  const stopConnection = connectionStore.start()
  taskStore.load()
  taskStore.startPolling()
  agentStore.load()
  agentStore.startPolling()
  projectStore.load()
  projectStore.startPolling()
  clusterStore.load()
  clusterStore.startPolling()
  // Seed PR data so project cards show open-PR counts before the GitHub page is
  // ever opened. The GitHub page owns the live polling.
  reviewStore.load()

  // Patch only the affected task per event instead of reloading the whole list.
  const onTaskEvent = makeTaskEventCoalescer()
  const unsubTaskCreated = EventsOn(ev.TaskCreated, (p: unknown) => onTaskEvent(String(p ?? ''), 'change'))
  const unsubTaskUpdated = EventsOn(ev.TaskUpdated, (p: unknown) => onTaskEvent(String(p ?? ''), 'change'))
  const unsubTaskDeleted = EventsOn(ev.TaskDeleted, (p: unknown) => onTaskEvent(String(p ?? ''), 'delete'))
  const unsubTasks = (): void => {
    unsubTaskCreated()
    unsubTaskUpdated()
    unsubTaskDeleted()
  }
  notificationStore.load()
  const unsubNotif = notificationStore.listen((taskId) => navStore.navigate({ kind: 'task-detail', taskId }))
  bgopStore.load()
  const unsubBgops = bgopStore.listen()
  const unsubDegraded = EventsOn(ev.StartupDegraded, (w: DegradedWarning) => hooks.onDegraded(w))

  // Seed provider health snapshot on mount then listen for flips.
  ProviderHealthEnabled().then((enabled) => {
    if (!enabled) return
    GetProviderHealth().then((list) => {
      const next: Record<string, ProviderHealth> = {}
      for (const p of list ?? []) next[p.provider] = p as ProviderHealth
      hooks.onProviderHealthSnapshot(next)
    }).catch(() => {})
  }).catch(() => {})

  const unsubProviderHealth = EventsOn(ev.ProviderHealth, (p: ProviderHealth) => {
    if (!p?.provider) return
    hooks.onProviderHealth(p)
  })
  const unsubQuit = EventsOn(ev.AppQuitConfirm, () => hooks.onQuitConfirm())

  return () => {
    stopConnection()
    unsubTasks()
    unsubNotif()
    unsubBgops()
    unsubDegraded()
    unsubProviderHealth()
    unsubQuit()
    taskStore.stopPolling()
    agentStore.stopPolling()
    projectStore.stopPolling()
    clusterStore.stopPolling()
  }
}
