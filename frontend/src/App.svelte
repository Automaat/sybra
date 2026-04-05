<script lang="ts">
  import { Navigation, AppBar } from '@skeletonlabs/skeleton-svelte'
  import { EventsOn } from '../wailsjs/runtime/runtime.js'
  import * as ev from './lib/events.js'
  import { taskStore } from './stores/tasks.svelte.js'
  import { agentStore } from './stores/agents.svelte.js'
  import { projectStore } from './stores/projects.svelte.js'
  import TaskList from './pages/TaskList.svelte'
  import TaskDetail from './pages/TaskDetail.svelte'
  import AgentList from './pages/AgentList.svelte'
  import AgentDetail from './pages/AgentDetail.svelte'
  import ProjectList from './pages/ProjectList.svelte'
  import ProjectDetail from './pages/ProjectDetail.svelte'
  import CreateTaskDialog from './components/CreateTaskDialog.svelte'
  import CreateProjectDialog from './components/CreateProjectDialog.svelte'
  import QuickAddTask from './components/QuickAddTask.svelte'
  import ToastContainer from './components/ToastContainer.svelte'
  import { notificationStore } from './stores/notifications.svelte.js'
  import Dashboard from './pages/Dashboard.svelte'
  import TmuxSessions from './pages/TmuxSessions.svelte'
  import Orchestrator from './pages/Orchestrator.svelte'
  import GitHub from './pages/GitHub.svelte'
  import Stats from './pages/Stats.svelte'
  import PlanReviews from './pages/PlanReviews.svelte'
  import Settings from './pages/Settings.svelte'

  type Page =
    | { kind: 'dashboard' }
    | { kind: 'task-list' }
    | { kind: 'task-detail'; taskId: string }
    | { kind: 'project-list' }
    | { kind: 'project-detail'; projectId: string }
    | { kind: 'agent-list' }
    | { kind: 'agent-detail'; agentId: string }
    | { kind: 'orchestrator' }
    | { kind: 'tmux' }
    | { kind: 'github' }
    | { kind: 'stats' }
    | { kind: 'plan-reviews' }
    | { kind: 'settings' }

  let page = $state<Page>({ kind: 'dashboard' })
  let dialogOpen = $state(false)
  let projectDialogOpen = $state(false)
  let quickAddOpen = $state(false)
  let quitConfirmVisible = $state(false)
  let quitConfirmTimer: ReturnType<typeof setTimeout> | null = null

  const pageTitle = $derived(
    page.kind === 'dashboard' ? 'Dashboard' :
    page.kind === 'task-list' ? 'Tasks' :
    page.kind === 'task-detail' ? 'Task Detail' :
    page.kind === 'project-list' ? 'Projects' :
    page.kind === 'project-detail' ? 'Project Detail' :
    page.kind === 'agent-list' ? 'Agents' :
    page.kind === 'orchestrator' ? 'Orchestrator' :
    page.kind === 'tmux' ? 'Tmux Sessions' :
    page.kind === 'github' ? 'GitHub' :
    page.kind === 'stats' ? 'Stats' :
    page.kind === 'plan-reviews' ? 'Plan Reviews' :
    page.kind === 'settings' ? 'Settings' :
    'Agent Detail'
  )

  function onEvents(events: string[], handler: () => void): () => void {
    const unsubs = events.map(e => EventsOn(e, handler))
    return () => unsubs.forEach(u => u())
  }

  $effect(() => {
    taskStore.load()
    taskStore.startPolling()
    agentStore.load()
    agentStore.startPolling()
    projectStore.load()
    projectStore.startPolling()

    const unsubTasks = onEvents([ev.TaskCreated, ev.TaskUpdated, ev.TaskDeleted], () => taskStore.load())
    notificationStore.load()
    const unsubNotif = notificationStore.listen()
    const unsubQuit = EventsOn(ev.AppQuitConfirm, () => {
      quitConfirmVisible = true
      if (quitConfirmTimer) clearTimeout(quitConfirmTimer)
      quitConfirmTimer = setTimeout(() => { quitConfirmVisible = false }, 3000)
    })
    function handleKeydown(e: KeyboardEvent) {
      if (e.metaKey && (e.key === '=' || e.key === '+')) {
        e.preventDefault()
        const current = parseFloat(document.documentElement.style.zoom || '1')
        document.documentElement.style.zoom = String(Math.min(current + 0.1, 2))
      }
      if (e.metaKey && e.key === '-') {
        e.preventDefault()
        const current = parseFloat(document.documentElement.style.zoom || '1')
        document.documentElement.style.zoom = String(Math.max(current - 0.1, 0.5))
      }
      if (e.metaKey && e.key === '0') {
        e.preventDefault()
        document.documentElement.style.zoom = '1'
      }
      if (e.metaKey && e.key === 'n') {
        e.preventDefault()
        quickAddOpen = true
      }
      if (e.metaKey && e.key === '1') {
        e.preventDefault()
        page = { kind: 'dashboard' }
      }
      if (e.metaKey && e.key === '2') {
        e.preventDefault()
        page = { kind: 'task-list' }
      }
      if (e.metaKey && e.key === '3') {
        e.preventDefault()
        page = { kind: 'project-list' }
      }
      if (e.metaKey && e.key === '4') {
        e.preventDefault()
        page = { kind: 'agent-list' }
      }
      if (e.metaKey && e.key === '5') {
        e.preventDefault()
        page = { kind: 'orchestrator' }
      }
      if (e.metaKey && e.key === '6') {
        e.preventDefault()
        page = { kind: 'tmux' }
      }
      if (e.metaKey && e.key === '7') {
        e.preventDefault()
        page = { kind: 'github' }
      }
      if (e.metaKey && e.key === '8') {
        e.preventDefault()
        page = { kind: 'plan-reviews' }
      }
      if (e.metaKey && e.key === '9') {
        e.preventDefault()
        page = { kind: 'stats' }
      }
      if (e.metaKey && e.key === ',') {
        e.preventDefault()
        page = { kind: 'settings' }
      }
    }
    window.addEventListener('keydown', handleKeydown)

    return () => {
      unsubTasks()
      unsubNotif()
      unsubQuit()
      if (quitConfirmTimer) clearTimeout(quitConfirmTimer)
      taskStore.stopPolling()
      agentStore.stopPolling()
      projectStore.stopPolling()
      window.removeEventListener('keydown', handleKeydown)
    }
  })
</script>

<div class="flex h-full">
  <Navigation layout="rail">
    <Navigation.Header>
      <span class="p-2 text-lg font-bold">S</span>
    </Navigation.Header>
    <Navigation.Content>
      <Navigation.Trigger
        onclick={() => (page = { kind: 'dashboard' })}
        data-active={page.kind === 'dashboard' || undefined}
      >
        <svg class="h-5 w-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 6a2 2 0 012-2h2a2 2 0 012 2v2a2 2 0 01-2 2H6a2 2 0 01-2-2V6zM14 6a2 2 0 012-2h2a2 2 0 012 2v2a2 2 0 01-2 2h-2a2 2 0 01-2-2V6zM4 16a2 2 0 012-2h2a2 2 0 012 2v2a2 2 0 01-2 2H6a2 2 0 01-2-2v-2zM14 16a2 2 0 012-2h2a2 2 0 012 2v2a2 2 0 01-2 2h-2a2 2 0 01-2-2v-2z" />
        </svg>
        <Navigation.TriggerText>Dashboard</Navigation.TriggerText>
      </Navigation.Trigger>
      <Navigation.Trigger
        onclick={() => (page = { kind: 'task-list' })}
        data-active={page.kind === 'task-list' || page.kind === 'task-detail' || undefined}
      >
        <svg class="h-5 w-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 5H7a2 2 0 00-2 2v12a2 2 0 002 2h10a2 2 0 002-2V7a2 2 0 00-2-2h-2M9 5a2 2 0 002 2h2a2 2 0 002-2M9 5a2 2 0 012-2h2a2 2 0 012 2" />
        </svg>
        <Navigation.TriggerText>Board</Navigation.TriggerText>
      </Navigation.Trigger>
      <Navigation.Trigger
        onclick={() => (page = { kind: 'project-list' })}
        data-active={page.kind === 'project-list' || page.kind === 'project-detail' || undefined}
      >
        <svg class="h-5 w-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 7v10a2 2 0 002 2h14a2 2 0 002-2V9a2 2 0 00-2-2h-6l-2-2H5a2 2 0 00-2 2z" />
        </svg>
        <Navigation.TriggerText>Projects</Navigation.TriggerText>
      </Navigation.Trigger>
      <Navigation.Trigger
        onclick={() => (page = { kind: 'agent-list' })}
        data-active={page.kind === 'agent-list' || page.kind === 'agent-detail' || undefined}
      >
        <svg class="h-5 w-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9.75 17L9 20l-1 1h8l-1-1-.75-3M3 13h18M5 17h14a2 2 0 002-2V5a2 2 0 00-2-2H5a2 2 0 00-2 2v10a2 2 0 002 2z" />
        </svg>
        <Navigation.TriggerText>Agents</Navigation.TriggerText>
      </Navigation.Trigger>
      <Navigation.Trigger
        onclick={() => (page = { kind: 'orchestrator' })}
        data-active={page.kind === 'orchestrator' || undefined}
      >
        <svg class="h-5 w-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M13 10V3L4 14h7v7l9-11h-7z" />
        </svg>
        <Navigation.TriggerText>Orchestrator</Navigation.TriggerText>
      </Navigation.Trigger>
      <Navigation.Trigger
        onclick={() => (page = { kind: 'tmux' })}
        data-active={page.kind === 'tmux' || undefined}
      >
        <svg class="h-5 w-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M8 9l3 3-3 3m5 0h3M5 20h14a2 2 0 002-2V6a2 2 0 00-2-2H5a2 2 0 00-2 2v12a2 2 0 002 2z" />
        </svg>
        <Navigation.TriggerText>Tmux</Navigation.TriggerText>
      </Navigation.Trigger>
      <Navigation.Trigger
        onclick={() => (page = { kind: 'github' })}
        data-active={page.kind === 'github' || undefined}
      >
        <svg class="h-5 w-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 2C6.477 2 2 6.477 2 12c0 4.42 2.865 8.166 6.839 9.489.5.092.682-.217.682-.482 0-.237-.009-.866-.013-1.7-2.782.604-3.369-1.341-3.369-1.341-.454-1.155-1.11-1.462-1.11-1.462-.908-.62.069-.608.069-.608 1.003.07 1.531 1.03 1.531 1.03.892 1.529 2.341 1.087 2.91.831.092-.646.35-1.086.636-1.337-2.22-.253-4.555-1.11-4.555-4.943 0-1.091.39-1.984 1.029-2.683-.103-.253-.446-1.27.098-2.647 0 0 .84-.269 2.75 1.025A9.578 9.578 0 0112 6.836a9.59 9.59 0 012.504.337c1.909-1.294 2.747-1.025 2.747-1.025.546 1.377.203 2.394.1 2.647.64.699 1.028 1.592 1.028 2.683 0 3.842-2.339 4.687-4.566 4.935.359.309.678.919.678 1.852 0 1.336-.012 2.415-.012 2.743 0 .267.18.578.688.48C19.138 20.163 22 16.418 22 12c0-5.523-4.477-10-10-10z" />
        </svg>
        <Navigation.TriggerText>GitHub</Navigation.TriggerText>
      </Navigation.Trigger>
      <Navigation.Trigger
        onclick={() => (page = { kind: 'plan-reviews' })}
        data-active={page.kind === 'plan-reviews' || undefined}
      >
        <div class="relative">
          <svg class="h-5 w-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 5H7a2 2 0 00-2 2v12a2 2 0 002 2h10a2 2 0 002-2V7a2 2 0 00-2-2h-2M9 5a2 2 0 002 2h2a2 2 0 002-2M9 5a2 2 0 012-2h2a2 2 0 012 2m-6 9l2 2 4-4" />
          </svg>
          {#if taskStore.byStatus('plan-review').length > 0}
            <span class="absolute -right-1 -top-1 flex h-3.5 w-3.5 items-center justify-center rounded-full bg-warning-500 text-[9px] font-bold text-white">{taskStore.byStatus('plan-review').length}</span>
          {/if}
        </div>
        <Navigation.TriggerText>Reviews</Navigation.TriggerText>
      </Navigation.Trigger>
      <Navigation.Trigger
        onclick={() => (page = { kind: 'stats' })}
        data-active={page.kind === 'stats' || undefined}
      >
        <svg class="h-5 w-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 19v-6a2 2 0 00-2-2H5a2 2 0 00-2 2v6a2 2 0 002 2h2a2 2 0 002-2zm0 0V9a2 2 0 012-2h2a2 2 0 012 2v10m-6 0a2 2 0 002 2h2a2 2 0 002-2m0 0V5a2 2 0 012-2h2a2 2 0 012 2v14a2 2 0 01-2 2h-2a2 2 0 01-2-2z" />
        </svg>
        <Navigation.TriggerText>Stats</Navigation.TriggerText>
      </Navigation.Trigger>
      <Navigation.Trigger
        onclick={() => (page = { kind: 'settings' })}
        data-active={page.kind === 'settings' || undefined}
        title="Settings (Cmd+,)"
      >
        <svg class="h-5 w-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M10.325 4.317c.426-1.756 2.924-1.756 3.35 0a1.724 1.724 0 002.573 1.066c1.543-.94 3.31.826 2.37 2.37a1.724 1.724 0 001.065 2.572c1.756.426 1.756 2.924 0 3.35a1.724 1.724 0 00-1.066 2.573c.94 1.543-.826 3.31-2.37 2.37a1.724 1.724 0 00-2.572 1.065c-.426 1.756-2.924 1.756-3.35 0a1.724 1.724 0 00-2.573-1.066c-1.543.94-3.31-.826-2.37-2.37a1.724 1.724 0 00-1.065-2.572c-1.756-.426-1.756-2.924 0-3.35a1.724 1.724 0 001.066-2.573c-.94-1.543.826-3.31 2.37-2.37.996.608 2.296.07 2.572-1.065z" />
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 12a3 3 0 11-6 0 3 3 0 016 0z" />
        </svg>
        <Navigation.TriggerText>Settings</Navigation.TriggerText>
      </Navigation.Trigger>
    </Navigation.Content>
  </Navigation>

  <div class="flex flex-1 flex-col overflow-hidden">
    <AppBar>
      <AppBar.Toolbar>
        <AppBar.Lead>
          <h2 class="text-lg font-semibold">{pageTitle}</h2>
        </AppBar.Lead>
        <AppBar.Trail>
          <button
            type="button"
            class="rounded-lg bg-primary-500 px-3 py-1.5 text-sm font-medium text-white hover:bg-primary-600"
            onclick={() => (dialogOpen = true)}
            title="New Task (Cmd+N)"
          >
            + New Task
          </button>
        </AppBar.Trail>
      </AppBar.Toolbar>
    </AppBar>

    <main class="flex-1 overflow-y-auto">
      {#if page.kind === 'dashboard'}
        <Dashboard
          onviewagent={(id) => (page = { kind: 'agent-detail', agentId: id })}
        />
      {:else if page.kind === 'task-list'}
        <TaskList onselect={(id) => (page = { kind: 'task-detail', taskId: id })} />
      {:else if page.kind === 'task-detail'}
        <TaskDetail
          taskId={page.taskId}
          onback={() => (page = { kind: 'task-list' })}
          onviewagent={(id) => (page = { kind: 'agent-detail', agentId: id })}
          ondelete={() => (page = { kind: 'task-list' })}
          onreviewplan={() => (page = { kind: 'plan-reviews' })}
        />
      {:else if page.kind === 'project-list'}
        <ProjectList
          onselect={(id) => (page = { kind: 'project-detail', projectId: id })}
          onadd={() => (projectDialogOpen = true)}
        />
      {:else if page.kind === 'project-detail'}
        <ProjectDetail
          projectId={page.projectId}
          onback={() => (page = { kind: 'project-list' })}
          onviewtask={(id) => (page = { kind: 'task-detail', taskId: id })}
        />
      {:else if page.kind === 'agent-list'}
        <AgentList onselect={(id) => (page = { kind: 'agent-detail', agentId: id })} />
      {:else if page.kind === 'agent-detail'}
        <AgentDetail
          agentId={page.agentId}
          onback={() => (page = { kind: 'agent-list' })}
          onviewtask={(id) => (page = { kind: 'task-detail', taskId: id })}
        />
      {:else if page.kind === 'orchestrator'}
        <Orchestrator />
      {:else if page.kind === 'tmux'}
        <TmuxSessions />
      {:else if page.kind === 'github'}
        <GitHub />
      {:else if page.kind === 'plan-reviews'}
        <PlanReviews onviewtask={(id) => (page = { kind: 'task-detail', taskId: id })} />
      {:else if page.kind === 'stats'}
        <Stats />
      {:else if page.kind === 'settings'}
        <Settings />
      {/if}
    </main>
  </div>
</div>

<CreateTaskDialog
  open={dialogOpen}
  onOpenChange={(open) => (dialogOpen = open)}
  oncreated={(id) => (page = { kind: 'task-detail', taskId: id })}
/>

<CreateProjectDialog
  open={projectDialogOpen}
  onOpenChange={(open) => (projectDialogOpen = open)}
  oncreated={(id) => (page = { kind: 'project-detail', projectId: id })}
/>

<QuickAddTask
  open={quickAddOpen}
  onclose={() => (quickAddOpen = false)}
/>

<ToastContainer onviewtask={(id) => (page = { kind: 'task-detail', taskId: id })} />

{#if quitConfirmVisible}
  <div class="fixed bottom-4 left-1/2 -translate-x-1/2 z-50 rounded-lg bg-surface-700 px-4 py-2 text-sm text-white shadow-lg">
    Press <kbd class="rounded bg-surface-500 px-1.5 py-0.5 font-mono text-xs">&#8984;Q</kbd> again to quit
  </div>
{/if}
