<script lang="ts">
  import { Navigation, AppBar } from '@skeletonlabs/skeleton-svelte'
  import { EventsOn } from '../wailsjs/runtime/runtime.js'
  import { taskStore } from './stores/tasks.svelte.js'
  import { agentStore } from './stores/agents.svelte.js'
  import TaskList from './pages/TaskList.svelte'
  import TaskDetail from './pages/TaskDetail.svelte'
  import AgentList from './pages/AgentList.svelte'
  import AgentDetail from './pages/AgentDetail.svelte'
  import CreateTaskDialog from './components/CreateTaskDialog.svelte'

  type Page =
    | { kind: 'task-list' }
    | { kind: 'task-detail'; taskId: string }
    | { kind: 'agent-list' }
    | { kind: 'agent-detail'; agentId: string }

  let page = $state<Page>({ kind: 'task-list' })
  let dialogOpen = $state(false)

  const pageTitle = $derived(
    page.kind === 'task-list' ? 'Tasks' :
    page.kind === 'task-detail' ? 'Task Detail' :
    page.kind === 'agent-list' ? 'Agents' :
    'Agent Detail'
  )

  $effect(() => {
    taskStore.load()
    agentStore.load()
    agentStore.startPolling()

    const unsub1 = EventsOn('task:created', () => taskStore.load())
    const unsub2 = EventsOn('task:updated', () => taskStore.load())
    const unsub3 = EventsOn('task:deleted', () => taskStore.load())

    return () => {
      unsub1()
      unsub2()
      unsub3()
      agentStore.stopPolling()
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
        onclick={() => (page = { kind: 'task-list' })}
        data-active={page.kind === 'task-list' || page.kind === 'task-detail' || undefined}
      >
        <svg class="h-5 w-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 5H7a2 2 0 00-2 2v12a2 2 0 002 2h10a2 2 0 002-2V7a2 2 0 00-2-2h-2M9 5a2 2 0 002 2h2a2 2 0 002-2M9 5a2 2 0 012-2h2a2 2 0 012 2" />
        </svg>
        <Navigation.TriggerText>Tasks</Navigation.TriggerText>
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
    </Navigation.Content>
  </Navigation>

  <div class="flex flex-1 flex-col overflow-hidden">
    <AppBar>
      <AppBar.Toolbar>
        <AppBar.Lead>
          <h2 class="text-lg font-semibold">{pageTitle}</h2>
        </AppBar.Lead>
        <AppBar.Trail>
          {#if page.kind === 'task-list'}
            <button
              type="button"
              class="rounded-lg bg-primary-500 px-3 py-1.5 text-sm font-medium text-white hover:bg-primary-600"
              onclick={() => (dialogOpen = true)}
            >
              + New Task
            </button>
          {/if}
        </AppBar.Trail>
      </AppBar.Toolbar>
    </AppBar>

    <main class="flex-1 overflow-y-auto">
      {#if page.kind === 'task-list'}
        <TaskList onselect={(id) => (page = { kind: 'task-detail', taskId: id })} />
      {:else if page.kind === 'task-detail'}
        <TaskDetail
          taskId={page.taskId}
          onback={() => (page = { kind: 'task-list' })}
          onviewagent={(id) => (page = { kind: 'agent-detail', agentId: id })}
        />
      {:else if page.kind === 'agent-list'}
        <AgentList onselect={(id) => (page = { kind: 'agent-detail', agentId: id })} />
      {:else if page.kind === 'agent-detail'}
        <AgentDetail
          agentId={page.agentId}
          onback={() => (page = { kind: 'agent-list' })}
          onviewtask={(id) => (page = { kind: 'task-detail', taskId: id })}
        />
      {/if}
    </main>
  </div>
</div>

<CreateTaskDialog
  open={dialogOpen}
  onOpenChange={(open) => (dialogOpen = open)}
  oncreated={(id) => (page = { kind: 'task-detail', taskId: id })}
/>
