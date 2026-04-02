<script lang="ts">
  import { Navigation, AppBar } from '@skeletonlabs/skeleton-svelte'
  import { EventsOn } from '../wailsjs/runtime/runtime.js'
  import { taskStore } from './stores/tasks.svelte.js'
  import TaskList from './pages/TaskList.svelte'
  import TaskDetail from './pages/TaskDetail.svelte'
  import CreateTaskDialog from './components/CreateTaskDialog.svelte'

  type Page = { kind: 'task-list' } | { kind: 'task-detail'; taskId: string }

  let page = $state<Page>({ kind: 'task-list' })
  let dialogOpen = $state(false)

  const pageTitle = $derived(page.kind === 'task-list' ? 'Tasks' : 'Task Detail')

  $effect(() => {
    taskStore.load()

    const unsub1 = EventsOn('task:created', () => taskStore.load())
    const unsub2 = EventsOn('task:updated', () => taskStore.load())
    const unsub3 = EventsOn('task:deleted', () => taskStore.load())

    return () => {
      unsub1()
      unsub2()
      unsub3()
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
        data-active={page.kind === 'task-list' || undefined}
      >
        <svg class="h-5 w-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 5H7a2 2 0 00-2 2v12a2 2 0 002 2h10a2 2 0 002-2V7a2 2 0 00-2-2h-2M9 5a2 2 0 002 2h2a2 2 0 002-2M9 5a2 2 0 012-2h2a2 2 0 012 2" />
        </svg>
        <Navigation.TriggerText>Tasks</Navigation.TriggerText>
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
        <TaskDetail taskId={page.taskId} onback={() => (page = { kind: 'task-list' })} />
      {/if}
    </main>
  </div>
</div>

<CreateTaskDialog
  open={dialogOpen}
  onOpenChange={(open) => (dialogOpen = open)}
  oncreated={(id) => (page = { kind: 'task-detail', taskId: id })}
/>
