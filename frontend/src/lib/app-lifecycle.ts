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

function onEvents(events: string[], handler: () => void): () => void {
  const unsubs = events.map((e) => EventsOn(e, handler))
  return () => unsubs.forEach((u) => u())
}

// Coalesce bursts of backend events into a single handler call. The backend
// can fire dozens of task:updated events per second when agents churn
// (restart-stale loops, rapid workflow advances, large headless sessions);
// a full taskStore.load() on every event re-builds the reactive Map and
// forces every kanban card to re-render, which saturates the WebKit main
// thread and freezes the UI even though the Go side is idle.
function debounced(fn: () => void, wait = 150): () => void {
  let timer: ReturnType<typeof setTimeout> | null = null
  let lastInvoke = 0
  return () => {
    const now = Date.now()
    if (now - lastInvoke >= wait && timer === null) {
      lastInvoke = now
      fn()
      return
    }
    if (timer !== null) clearTimeout(timer)
    timer = setTimeout(() => {
      lastInvoke = Date.now()
      timer = null
      fn()
    }, wait)
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

  const reloadTasks = debounced(() => taskStore.load(), 150)
  const unsubTasks = onEvents([ev.TaskCreated, ev.TaskUpdated, ev.TaskDeleted], reloadTasks)
  notificationStore.load()
  const unsubNotif = notificationStore.listen()
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
  }
}
