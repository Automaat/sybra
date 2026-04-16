<script lang="ts">
  import { untrack } from 'svelte'
  import { EventsOn, GetProviderHealth, ProviderHealthEnabled } from '$lib/api'
  import * as ev from './lib/events.js'
  import { taskStore } from './stores/tasks.svelte.js'
  import { agentStore } from './stores/agents.svelte.js'
  import { projectStore } from './stores/projects.svelte.js'
  import { notificationStore } from './stores/notifications.svelte.js'
  import { bgopStore } from './stores/bgops.svelte.js'
  import { navStore, type Page } from './lib/navigation.svelte.js'
  import { viewport } from './lib/viewport.svelte.js'
  import { connectionStore } from './stores/connection.svelte.js'
  import AppShell from './components/shell/AppShell.svelte'
  import TaskList from './pages/TaskList.svelte'
  import TaskDetail from './pages/TaskDetail.svelte'
  import Agents from './pages/Agents.svelte'
  import AgentDetail from './pages/AgentDetail.svelte'
  import ProjectList from './pages/ProjectList.svelte'
  import ProjectDetail from './pages/ProjectDetail.svelte'
  import Dashboard from './pages/Dashboard.svelte'
  import GitHub from './pages/GitHub.svelte'
  import Stats from './pages/Stats.svelte'
  import Reviews from './pages/Reviews.svelte'
  import Settings from './pages/Settings.svelte'
  import ChatList from './pages/ChatList.svelte'
  import ChatDetail from './pages/ChatDetail.svelte'
  import WorkflowList from './pages/WorkflowList.svelte'
  import WorkflowDetail from './pages/WorkflowDetail.svelte'
  import CreateTaskDialog from './components/CreateTaskDialog.svelte'
  import CreateProjectDialog from './components/CreateProjectDialog.svelte'
  import QuickAddTask from './components/QuickAddTask.svelte'
  import ToastContainer from './components/ToastContainer.svelte'
  import CommandPalette from './components/CommandPalette.svelte'
  import type { PaletteCtx } from './lib/palette-commands.js'
  import KeyboardHelp from './components/KeyboardHelp.svelte'
  import { Cloud, AlertTriangle } from '@lucide/svelte'

  const paletteCtx: PaletteCtx = {
    navigate: (page) => { commandPaletteOpen = false; navStore.reset(page) },
    openNewTask: () => { commandPaletteOpen = false; dialogOpen = true },
    openNewProject: () => { commandPaletteOpen = false; projectDialogOpen = true },
    openKeyboardHelp: () => { commandPaletteOpen = false; helpOpen = true },
  }

  type DegradedWarning = { subsystem: string; reason: string }
  let degradedWarnings = $state<DegradedWarning[]>([])

  type ProviderHealth = {
    provider: string
    healthy: boolean
    reason: string
    detail?: string
    lastCheck?: string
    ratelimitedUntil?: string
    failoverActive?: boolean
  }
  let providerHealth = $state<Record<string, ProviderHealth>>({})
  const unhealthyProviders = $derived(
    Object.values(providerHealth).filter(p => !p.healthy && p.reason !== 'disabled' && p.reason !== 'unknown')
  )

  let dialogOpen = $state(false)
  let projectDialogOpen = $state(false)
  let quickAddOpen = $state(false)
  let commandPaletteOpen = $state(false)
  let helpOpen = $state(false)
  let quitConfirmVisible = $state(false)
  let quitConfirmTimer: ReturnType<typeof setTimeout> | null = null

  const primaryAction = $derived.by(() => {
    const k = navStore.page.kind
    if (k === 'task-list' || k === 'dashboard') return { label: 'New Task', run: () => (dialogOpen = true) }
    if (k === 'project-list') return { label: 'New Project', run: () => (projectDialogOpen = true) }
    return null
  })

  function onEvents(events: string[], handler: () => void): () => void {
    const unsubs = events.map(e => EventsOn(e, handler))
    return () => unsubs.forEach(u => u())
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

  $effect(() => {
    // This effect is lifecycle (mount/unmount), not reactive: it must NOT track
    // any reads from the store loads it kicks off, otherwise the writes those
    // loads make to $state stores would re-run this effect, which would cancel
    // every subscription and EventSource connection and re-create them in a
    // tight loop (~60×/s in the wild — caused full UI flicker on the web build).
    const cleanup = untrack(() => {
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
      const unsubDegraded = EventsOn(ev.StartupDegraded, (w: DegradedWarning) => {
        degradedWarnings = [...degradedWarnings, w]
      })
      // Seed provider health snapshot on mount then listen for flips.
      ProviderHealthEnabled().then(enabled => {
        if (!enabled) return
        GetProviderHealth().then(list => {
          const next: Record<string, ProviderHealth> = {}
          for (const p of list ?? []) next[p.provider] = p as ProviderHealth
          providerHealth = next
        }).catch(() => {})
      }).catch(() => {})
      const unsubProviderHealth = EventsOn(ev.ProviderHealth, (p: ProviderHealth) => {
        if (!p?.provider) return
        providerHealth = { ...providerHealth, [p.provider]: p }
      })
      const unsubQuit = EventsOn(ev.AppQuitConfirm, () => {
        quitConfirmVisible = true
        if (quitConfirmTimer) clearTimeout(quitConfirmTimer)
        quitConfirmTimer = setTimeout(() => { quitConfirmVisible = false }, 3000)
      })
      return { stopConnection, unsubTasks, unsubNotif, unsubDegraded, unsubProviderHealth, unsubQuit, unsubBgops }
    })
    const { stopConnection, unsubTasks, unsubNotif, unsubDegraded, unsubProviderHealth, unsubQuit, unsubBgops } = cleanup

    // Keyboard shortcuts only on devices with a fine pointer (mouse/keyboard).
    // Touch-only devices (iPhone, iPad without keyboard) skip listener entirely.
    const hasFinePointer = typeof window !== 'undefined' && window.matchMedia?.('(pointer: fine)').matches
    let removeKeyHandler: (() => void) | undefined
    if (hasFinePointer) {
      let pendingG = false
      let gTimer: ReturnType<typeof setTimeout> | null = null

      function handleKeydown(e: KeyboardEvent) {
        // Clear G-chord when a modifier key combo fires
        if ((e.metaKey || e.ctrlKey || e.altKey) && pendingG) {
          pendingG = false
          if (gTimer) { clearTimeout(gTimer); gTimer = null }
        }

        if (e.metaKey && e.key === 'n') {
          e.preventDefault()
          quickAddOpen = true
        }
        if (e.metaKey && e.key === 'k') {
          e.preventDefault()
          commandPaletteOpen = true
        }
        if (e.metaKey && e.key === '/') {
          e.preventDefault()
          helpOpen = true
        }
        if (e.metaKey && e.key === '1') {
          e.preventDefault()
          navStore.reset({ kind: 'dashboard' })
        }
        if (e.metaKey && e.key === '2') {
          e.preventDefault()
          navStore.reset({ kind: 'task-list' })
        }
        if (e.metaKey && e.key === '3') {
          e.preventDefault()
          navStore.reset({ kind: 'project-list' })
        }
        if (e.metaKey && e.key === '4') {
          e.preventDefault()
          navStore.reset({ kind: 'agents' })
        }
        if (e.metaKey && e.key === '5') {
          e.preventDefault()
          navStore.reset({ kind: 'github' })
        }
        if (e.metaKey && e.key === '6') {
          e.preventDefault()
          navStore.reset({ kind: 'reviews' })
        }
        if (e.metaKey && e.key === '7') {
          e.preventDefault()
          navStore.reset({ kind: 'stats' })
        }
        if (e.metaKey && e.key === ',') {
          e.preventDefault()
          navStore.reset({ kind: 'settings' })
        }
        if (e.metaKey && e.key === 'f') {
          e.preventDefault()
          navStore.reset({ kind: 'task-list' })
          requestAnimationFrame(() => window.dispatchEvent(new CustomEvent('focus-search')))
        }

        // G-chord navigation: bare keypresses only, not in input/textarea/contenteditable
        if (!e.metaKey && !e.ctrlKey && !e.altKey && !e.shiftKey) {
          const target = e.target as HTMLElement
          const inInput = target.tagName === 'INPUT' || target.tagName === 'TEXTAREA' || target.isContentEditable
          if (!inInput) {
            if (pendingG) {
              pendingG = false
              if (gTimer) { clearTimeout(gTimer); gTimer = null }
              if (e.key === 'i') { e.preventDefault(); navStore.reset({ kind: 'task-list' }) }
              else if (e.key === 'a') { e.preventDefault(); navStore.reset({ kind: 'task-list', filter: 'in-progress' }) }
              else if (e.key === 'p') { e.preventDefault(); navStore.reset({ kind: 'project-list' }) }
              else if (e.key === 's') { e.preventDefault(); navStore.reset({ kind: 'settings' }) }
              // unmapped letters: clear pendingG silently, no navigation
              return
            }
            if (e.key === 'g') {
              e.preventDefault()
              pendingG = true
              gTimer = setTimeout(() => { pendingG = false; gTimer = null }, 1500)
              return
            }
          }
        }
      }
      window.addEventListener('keydown', handleKeydown)
      removeKeyHandler = () => {
        window.removeEventListener('keydown', handleKeydown)
        if (gTimer) clearTimeout(gTimer)
      }
    }

    return () => {
      stopConnection()
      unsubTasks()
      unsubNotif()
      unsubBgops()
      unsubDegraded()
      unsubProviderHealth()
      unsubQuit()
      if (quitConfirmTimer) clearTimeout(quitConfirmTimer)
      taskStore.stopPolling()
      agentStore.stopPolling()
      projectStore.stopPolling()
      removeKeyHandler?.()
    }
  })

  function navTaskDetail(id: string) { navStore.navigate({ kind: 'task-detail', taskId: id }) }
  function navAgentDetail(id: string) { navStore.navigate({ kind: 'agent-detail', agentId: id }) }
  function navChatDetail(id: string) { navStore.navigate({ kind: 'chat-detail', agentId: id }) }
  function navProjectDetail(id: string) { navStore.navigate({ kind: 'project-detail', projectId: id }) }
  function navWorkflowDetail(id: string) { navStore.navigate({ kind: 'workflow-detail', workflowId: id }) }
</script>

<AppShell onsearch={() => (commandPaletteOpen = true)} {primaryAction}>
  {#if !connectionStore.online}
    <div class="flex shrink-0 items-center gap-2 border-b border-warning-600 bg-warning-800/90 px-4 py-2 text-sm text-warning-100">
      <Cloud size={16} class="shrink-0" />
      <span>
        <strong>Offline</strong> — task board is read-only.
        {connectionStore.networkOnline ? 'Backend unreachable.' : 'No network connection.'}
        Agents cannot start; GitHub sync will resume when reconnected.
      </span>
    </div>
  {/if}

  {#if unhealthyProviders.length > 0}
    <div class="flex shrink-0 flex-col gap-0.5">
      {#each unhealthyProviders as p (p.provider)}
        <div class="flex items-center gap-2 bg-error-800/90 border-b border-error-600 px-4 py-2 text-error-100 text-sm">
          <AlertTriangle size={16} class="shrink-0" />
          <span>
            <strong>{p.provider}</strong> unavailable — {p.reason}
            {#if p.ratelimitedUntil}· until {new Date(p.ratelimitedUntil).toLocaleTimeString()}{/if}
            {#if p.failoverActive}· failing over to peer{/if}
          </span>
        </div>
      {/each}
    </div>
  {/if}

  {#if degradedWarnings.length > 0}
    <div class="flex shrink-0 flex-col gap-0.5">
      {#each degradedWarnings as w, i (w.subsystem)}
        <div class="flex items-center gap-2 bg-warning-800/90 border-b border-warning-600 px-4 py-2 text-warning-100 text-sm">
          <AlertTriangle size={16} class="shrink-0" />
          <span><strong>{w.subsystem}</strong> degraded — {w.reason}</span>
          <button
            type="button"
            class="ml-auto opacity-60 hover:opacity-100 text-xs"
            onclick={() => { degradedWarnings = degradedWarnings.filter((_, j) => j !== i) }}
            aria-label="Dismiss"
          >✕</button>
        </div>
      {/each}
    </div>
  {/if}

  <main class="flex min-h-0 flex-1 flex-col overflow-y-auto">
    {#if navStore.page.kind === 'dashboard'}
      <Dashboard onviewagent={navAgentDetail} />
    {:else if navStore.page.kind === 'task-list'}
      <TaskList onselect={navTaskDetail} filter={navStore.page.filter} />
    {:else if navStore.page.kind === 'task-detail'}
      <TaskDetail
        taskId={navStore.page.taskId}
        onback={() => navStore.back()}
        onviewagent={navAgentDetail}
        ondelete={() => navStore.back()}
        onreviewplan={() => navStore.reset({ kind: 'reviews' })}
      />
    {:else if navStore.page.kind === 'project-list'}
      <ProjectList
        onselect={navProjectDetail}
        onadd={() => (projectDialogOpen = true)}
      />
    {:else if navStore.page.kind === 'project-detail'}
      <ProjectDetail
        projectId={navStore.page.projectId}
        onback={() => navStore.back()}
        onviewtask={navTaskDetail}
      />
    {:else if navStore.page.kind === 'chats'}
      <ChatList onselect={navChatDetail} />
    {:else if navStore.page.kind === 'chat-detail'}
      <ChatDetail
        agentId={navStore.page.agentId}
        onback={() => navStore.back()}
        onviewtask={navTaskDetail}
      />
    {:else if navStore.page.kind === 'agents'}
      <Agents
        initialTab={navStore.page.tab}
        onselect={navAgentDetail}
      />
    {:else if navStore.page.kind === 'agent-detail'}
      <AgentDetail
        agentId={navStore.page.agentId}
        onback={() => navStore.back()}
        onviewtask={navTaskDetail}
      />
    {:else if navStore.page.kind === 'github'}
      <GitHub />
    {:else if navStore.page.kind === 'reviews'}
      <Reviews onviewtask={navTaskDetail} />
    {:else if navStore.page.kind === 'stats'}
      <Stats />
    {:else if navStore.page.kind === 'workflows'}
      <WorkflowList onselect={navWorkflowDetail} />
    {:else if navStore.page.kind === 'workflow-detail'}
      <WorkflowDetail
        workflowId={navStore.page.workflowId}
        onback={() => navStore.back()}
      />
    {:else if navStore.page.kind === 'settings'}
      <Settings />
    {/if}
  </main>
</AppShell>

<CreateTaskDialog
  open={dialogOpen}
  onOpenChange={(open) => (dialogOpen = open)}
  oncreated={(id) => navStore.navigate({ kind: 'task-detail', taskId: id })}
/>

<CreateProjectDialog
  open={projectDialogOpen}
  onOpenChange={(open) => (projectDialogOpen = open)}
  oncreated={(id) => navStore.navigate({ kind: 'project-detail', projectId: id })}
/>

<QuickAddTask
  open={quickAddOpen}
  onclose={() => (quickAddOpen = false)}
/>

<CommandPalette
  open={commandPaletteOpen}
  onclose={() => (commandPaletteOpen = false)}
  ctx={paletteCtx}
/>

{#if !viewport.hasCoarsePointer}
  <KeyboardHelp
    open={helpOpen}
    onclose={() => (helpOpen = false)}
  />
{/if}

<ToastContainer onviewtask={navTaskDetail} />

{#if quitConfirmVisible}
  <div class="fixed bottom-4 left-1/2 -translate-x-1/2 z-50 rounded-lg bg-surface-700 px-4 py-2 text-sm text-white shadow-lg">
    Press <kbd class="rounded bg-surface-500 px-1.5 py-0.5 font-mono text-xs">&#8984;Q</kbd> again to quit
  </div>
{/if}
