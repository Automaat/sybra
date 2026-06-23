<script lang="ts">
  import { LayoutGrid, ClipboardList, Folder, MessageCircle, UserCircle, GitBranch, ClipboardCheck, LayoutDashboard, BarChart3, Settings, Archive } from '@lucide/svelte'
  import type { Component } from 'svelte'
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
    icon: Component<{ size?: number }>
    title?: string
    onclick: () => void
  }

  interface NavGroup {
    label: string
    items: NavItem[]
  }

  // Grouped so 11 destinations read as ~4 scannable sections instead of a flat
  // wall. Group labels avoid colliding with any item label. Settings is pinned
  // to the bottom. Every destination stays one click away.
  const groups: NavGroup[] = [
    {
      label: 'Work',
      items: [
        { kind: ['dashboard'], label: 'Dashboard', icon: LayoutGrid, onclick: () => navStore.reset({ kind: 'dashboard' }) },
        { kind: ['task-list', 'task-detail'], label: 'Board', icon: ClipboardList, onclick: () => navStore.reset({ kind: 'task-list' }) },
        { kind: ['reviews'], label: 'Reviews', icon: ClipboardCheck, onclick: () => navStore.reset({ kind: 'reviews' }) },
        { kind: ['logbook'], label: 'Logbook', icon: Archive, onclick: () => navStore.reset({ kind: 'logbook' }) },
      ],
    },
    {
      label: 'Sessions',
      items: [
        { kind: ['chats', 'chat-detail'], label: 'Chats', icon: MessageCircle, onclick: () => navStore.reset({ kind: 'chats' }) },
        { kind: ['agents', 'agent-detail'], label: 'Agents', icon: UserCircle, onclick: () => navStore.reset({ kind: 'agents' }) },
      ],
    },
    {
      label: 'Build',
      items: [
        { kind: ['project-list', 'project-detail'], label: 'Projects', icon: Folder, onclick: () => navStore.reset({ kind: 'project-list' }) },
        { kind: ['workflows', 'workflow-detail'], label: 'Workflows', icon: LayoutDashboard, onclick: () => navStore.reset({ kind: 'workflows' }) },
      ],
    },
    {
      label: 'Data',
      items: [
        { kind: ['github'], label: 'GitHub', icon: GitBranch, onclick: () => navStore.reset({ kind: 'github' }) },
        { kind: ['stats'], label: 'Stats', icon: BarChart3, onclick: () => navStore.reset({ kind: 'stats' }) },
      ],
    },
  ]

  const settingsItem: NavItem = {
    kind: ['settings'],
    label: 'Settings',
    icon: Settings,
    title: 'Settings (Cmd+,)',
    onclick: () => navStore.reset({ kind: 'settings' }),
  }
</script>

{#snippet navButton(item: NavItem)}
  {@const active = item.kind.includes(navStore.page.kind)}
  {@const Icon = item.icon}
  <button
    type="button"
    data-part="trigger"
    onclick={item.onclick}
    title={item.title ?? item.label}
    class="flex flex-col items-center gap-0.5 rounded-md px-1 py-1.5 text-[10px] font-medium transition-colors
      {active
        ? 'bg-primary-100 text-primary-700 dark:bg-primary-900/40 dark:text-primary-300'
        : 'text-surface-600 hover:bg-surface-200 dark:text-surface-400 dark:hover:bg-surface-700'}"
  >
    <div class="relative">
      <Icon size={18} />
      {#if item.label === 'Chats' && interactiveAgentCount > 0}
        <span class="absolute -right-1 -top-1 flex h-3 w-3 items-center justify-center rounded-full bg-primary-500 text-[8px] font-bold text-white">{interactiveAgentCount}</span>
      {:else if item.label === 'Agents' && runningAgentCount > 0}
        <span class="absolute -right-1 -top-1 flex h-3 w-3 items-center justify-center rounded-full bg-success-500 text-[8px] font-bold text-white">{runningAgentCount}</span>
      {:else if item.label === 'Reviews' && reviewCount > 0}
        <span class="absolute -right-1 -top-1 flex h-3 w-3 items-center justify-center rounded-full bg-warning-500 text-[8px] font-bold text-white">{reviewCount}</span>
      {/if}
    </div>
    <span class="leading-tight">{item.label}</span>
  </button>
{/snippet}

<nav class="flex h-full w-16 flex-col border-r border-surface-200 bg-surface-50 dark:border-surface-700 dark:bg-surface-900">
  <div class="flex shrink-0 items-center justify-center py-3">
    <span class="text-lg font-bold">S</span>
  </div>
  <div class="flex flex-1 flex-col gap-0.5 overflow-y-auto px-1 py-1">
    {#each groups as group, gi}
      {#if gi > 0}
        <div class="mx-2 mb-0.5 mt-1.5 border-t border-surface-200 dark:border-surface-700"></div>
      {/if}
      <span class="px-1 pb-0.5 text-center text-[8px] font-semibold uppercase tracking-wider text-surface-400">{group.label}</span>
      {#each group.items as item}
        {@render navButton(item)}
      {/each}
    {/each}
  </div>
  <div class="shrink-0 border-t border-surface-200 px-1 py-1 dark:border-surface-700">
    {@render navButton(settingsItem)}
  </div>
</nav>
