<script lang="ts">
  import { LayoutGrid, ClipboardList, Folder, MessageCircle, UserCircle, GitBranch, ClipboardCheck, LayoutDashboard, BarChart3, Settings, Archive } from '@lucide/svelte'
  import { navStore } from '../../lib/navigation.svelte.js'
  import { taskStore } from '../../stores/tasks.svelte.js'
  import { agentStore } from '../../stores/agents.svelte.js'

  const interactiveAgentCount = $derived(
    agentStore.list.filter(a => a.mode === 'interactive' && (a.state === 'running' || a.state === 'paused')).length
  )

  const runningAgentCount = $derived(
    agentStore.list.filter(a => a.state === 'running').length
  )

  const reviewCount = $derived(
    taskStore.byStatus('plan-review').length + taskStore.byStatus('test-plan-review').length
  )

  interface NavItem {
    kind: string[]
    label: string
    title?: string
    onclick: () => void
  }

  const items: NavItem[] = [
    { kind: ['dashboard'], label: 'Dashboard', onclick: () => navStore.reset({ kind: 'dashboard' }) },
    { kind: ['task-list', 'task-detail'], label: 'Board', onclick: () => navStore.reset({ kind: 'task-list' }) },
    { kind: ['project-list', 'project-detail'], label: 'Projects', onclick: () => navStore.reset({ kind: 'project-list' }) },
    { kind: ['chats', 'chat-detail'], label: 'Chats', onclick: () => navStore.reset({ kind: 'chats' }) },
    { kind: ['agents', 'agent-detail'], label: 'Agents', onclick: () => navStore.reset({ kind: 'agents' }) },
    { kind: ['github'], label: 'GitHub', onclick: () => navStore.reset({ kind: 'github' }) },
    { kind: ['reviews'], label: 'Reviews', onclick: () => navStore.reset({ kind: 'reviews' }) },
    { kind: ['logbook'], label: 'Logbook', onclick: () => navStore.reset({ kind: 'logbook' }) },
    { kind: ['workflows', 'workflow-detail'], label: 'Workflows', onclick: () => navStore.reset({ kind: 'workflows' }) },
    { kind: ['stats'], label: 'Stats', onclick: () => navStore.reset({ kind: 'stats' }) },
    { kind: ['settings'], label: 'Settings', title: 'Settings (Cmd+,)', onclick: () => navStore.reset({ kind: 'settings' }) },
  ]
</script>

<nav class="flex h-full w-16 flex-col border-r border-surface-200 bg-surface-50 dark:border-surface-700 dark:bg-surface-900">
  <div class="flex shrink-0 items-center justify-center py-3">
    <span class="text-lg font-bold">S</span>
  </div>
  <div class="flex flex-1 flex-col gap-0.5 overflow-y-auto px-1 py-1">
    {#each items as item}
      {@const active = item.kind.includes(navStore.page.kind)}
      <button
        type="button"
        onclick={item.onclick}
        title={item.title ?? item.label}
        class="flex flex-col items-center gap-0.5 rounded-md px-1 py-1.5 text-[10px] font-medium transition-colors
          {active
            ? 'bg-primary-100 text-primary-700 dark:bg-primary-900/40 dark:text-primary-300'
            : 'text-surface-600 hover:bg-surface-200 dark:text-surface-400 dark:hover:bg-surface-700'}"
      >
        <div class="relative">
          {#if item.label === 'Dashboard'}
            <LayoutGrid size={18} />
          {:else if item.label === 'Board'}
            <ClipboardList size={18} />
          {:else if item.label === 'Projects'}
            <Folder size={18} />
          {:else if item.label === 'Chats'}
            <MessageCircle size={18} />
            {#if interactiveAgentCount > 0}
              <span class="absolute -right-1 -top-1 flex h-3 w-3 items-center justify-center rounded-full bg-primary-500 text-[8px] font-bold text-white">{interactiveAgentCount}</span>
            {/if}
          {:else if item.label === 'Agents'}
            <UserCircle size={18} />
            {#if runningAgentCount > 0}
              <span class="absolute -right-1 -top-1 flex h-3 w-3 items-center justify-center rounded-full bg-success-500 text-[8px] font-bold text-white">{runningAgentCount}</span>
            {/if}
          {:else if item.label === 'GitHub'}
            <GitBranch size={18} />
          {:else if item.label === 'Reviews'}
            <ClipboardCheck size={18} />
            {#if reviewCount > 0}
              <span class="absolute -right-1 -top-1 flex h-3 w-3 items-center justify-center rounded-full bg-warning-500 text-[8px] font-bold text-white">{reviewCount}</span>
            {/if}
          {:else if item.label === 'Logbook'}
            <Archive size={18} />
          {:else if item.label === 'Workflows'}
            <LayoutDashboard size={18} />
          {:else if item.label === 'Stats'}
            <BarChart3 size={18} />
          {:else if item.label === 'Settings'}
            <Settings size={18} />
          {/if}
        </div>
        <span class="leading-tight">{item.label}</span>
      </button>
    {/each}
  </div>
</nav>
