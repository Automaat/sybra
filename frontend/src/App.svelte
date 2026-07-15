<script lang="ts">
  import { untrack } from 'svelte'
  import { navStore } from './lib/navigation.svelte.js'
  import { viewport } from './lib/viewport.svelte.js'
  import { connectionStore } from './stores/connection.svelte.js'
  import AppShell from './components/shell/AppShell.svelte'
  import CreateTaskDialog from './components/CreateTaskDialog.svelte'
  import CreateProjectDialog from './components/CreateProjectDialog.svelte'
  import QuickAddTask from './components/QuickAddTask.svelte'
  import ToastContainer from './components/ToastContainer.svelte'
  import CommandPalette from './components/CommandPalette.svelte'
  import type { PaletteCtx } from './lib/palette-commands.js'
  import KeyboardHelp from './components/KeyboardHelp.svelte'
  import AppWarningsBanner from './components/AppWarningsBanner.svelte'
  import PageRouter from './components/PageRouter.svelte'
  import { handleAppKeydown, type AppKeyAction } from './lib/app-keyboard.js'
  import { focusModeStore } from './lib/focus-mode.svelte.js'
  import { viewModeStore } from './lib/view-mode.svelte.js'
  import {
    startAppLifecycle,
    type ProviderHealth,
    type DegradedWarning,
  } from './lib/app-lifecycle.js'

  const paletteCtx: PaletteCtx = {
    navigate: (page) => { commandPaletteOpen = false; navStore.reset(page) },
    openNewTask: () => { commandPaletteOpen = false; dialogOpen = true },
    openNewProject: () => { commandPaletteOpen = false; projectDialogOpen = true },
    openKeyboardHelp: () => { commandPaletteOpen = false; helpOpen = true },
    toggleFocus: () => {
      commandPaletteOpen = false
      focusModeStore.toggle()
      if (focusModeStore.enabled) viewModeStore.set('list')
    },
  }

  // Focus mode leads with the list view, so re-apply it on startup since the
  // persisted view mode may be board.
  if (focusModeStore.enabled) viewModeStore.set('list')

  let degradedWarnings = $state<DegradedWarning[]>([])
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
  let sidebarTaskId = $state<string | null>(null)
  let focusedTaskIdFromList = $state<string | null>(null)

  const primaryAction = $derived.by(() => {
    const k = navStore.page.kind
    if (k === 'task-list') return { label: 'New Task', run: () => (dialogOpen = true) }
    if (k === 'project-list') return { label: 'New Project', run: () => (projectDialogOpen = true) }
    return null
  })

  $effect(() => {
    // This effect is lifecycle (mount/unmount), not reactive: it must NOT track
    // any reads from the store loads it kicks off, otherwise the writes those
    // loads make to $state stores would re-run this effect, which would cancel
    // every subscription and EventSource connection and re-create them in a
    // tight loop (~60×/s in the wild — caused full UI flicker on the web build).
    const stopLifecycle = untrack(() =>
      startAppLifecycle({
        onDegraded: (w) => { degradedWarnings = [...degradedWarnings, w] },
        onProviderHealthSnapshot: (snapshot) => { providerHealth = snapshot },
        onProviderHealth: (p) => { providerHealth = { ...providerHealth, [p.provider]: p } },
        onQuitConfirm: () => {
          quitConfirmVisible = true
          if (quitConfirmTimer) clearTimeout(quitConfirmTimer)
          quitConfirmTimer = setTimeout(() => { quitConfirmVisible = false }, 3000)
        },
      })
    )

    // URL-backed navigation only makes sense in the browser (deep links,
    // back/forward, refresh) — the desktop Wails webview has no address bar.
    const stopUrlRouting = import.meta.env.VITE_MODE === 'web'
      ? untrack(() => navStore.startUrlRouting())
      : undefined

    // Keyboard shortcuts only on devices with a fine pointer (mouse/keyboard).
    // Touch-only devices (iPhone, iPad without keyboard) skip listener entirely.
    const hasFinePointer = typeof window !== 'undefined' && window.matchMedia?.('(pointer: fine)').matches
    let removeKeyHandler: (() => void) | undefined
    if (hasFinePointer) {
      let pendingG = false
      let gTimer: ReturnType<typeof setTimeout> | null = null

      function applyAction(action: AppKeyAction) {
        switch (action.type) {
          case 'open-quickadd': quickAddOpen = true; break
          case 'open-palette': commandPaletteOpen = true; break
          case 'open-help': helpOpen = true; break
          case 'nav-reset': navStore.reset(action.page); break
          case 'toggle-view':
            window.dispatchEvent(new CustomEvent('toggle-view'))
            break
          case 'focus-search':
            if (action.target === 'renovate') {
              window.dispatchEvent(new CustomEvent('focus-renovate-search'))
            } else {
              navStore.reset({ kind: 'task-list' })
              requestAnimationFrame(() => window.dispatchEvent(new CustomEvent('focus-search')))
            }
            break
          case 'toggle-sidebar':
            if (focusedTaskIdFromList) {
              sidebarTaskId = sidebarTaskId === focusedTaskIdFromList ? null : focusedTaskIdFromList
            } else {
              sidebarTaskId = null
            }
            break
        }
      }

      function handleKeydown(e: KeyboardEvent) {
        const result = handleAppKeydown(e, { pendingG, currentPageKind: navStore.page.kind })
        if (result.preventDefault) e.preventDefault()
        if (result.action !== null) applyAction(result.action)
        if (result.nextPendingG !== pendingG) {
          if (gTimer) { clearTimeout(gTimer); gTimer = null }
          pendingG = result.nextPendingG
          if (pendingG) gTimer = setTimeout(() => { pendingG = false; gTimer = null }, 1500)
        }
      }
      window.addEventListener('keydown', handleKeydown)
      removeKeyHandler = () => {
        window.removeEventListener('keydown', handleKeydown)
        if (gTimer) clearTimeout(gTimer)
      }
    }

    return () => {
      stopLifecycle()
      stopUrlRouting?.()
      if (quitConfirmTimer) clearTimeout(quitConfirmTimer)
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
  <AppWarningsBanner
    online={connectionStore.online}
    networkOnline={connectionStore.networkOnline}
    unhealthyProviders={unhealthyProviders}
    degradedWarnings={degradedWarnings}
    ondismissDegraded={(i) => { degradedWarnings = degradedWarnings.filter((_, j) => j !== i) }}
  />

  <PageRouter
    sidebarTaskId={sidebarTaskId}
    onsidebarclose={() => (sidebarTaskId = null)}
    onfocusedtaskchange={(id) => (focusedTaskIdFromList = id)}
    navTaskDetail={navTaskDetail}
    navAgentDetail={navAgentDetail}
    navChatDetail={navChatDetail}
    navProjectDetail={navProjectDetail}
    navWorkflowDetail={navWorkflowDetail}
    onnewTask={() => (quickAddOpen = true)}
    onnewProject={() => (projectDialogOpen = true)}
    onselectTaskFromList={(id) => { sidebarTaskId = null; navTaskDetail(id) }}
  />
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
