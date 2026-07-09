<script lang="ts">
  import { ClipboardList, Folder, MessageCircle, UserCircle, GitBranch, ClipboardCheck, LayoutDashboard, BarChart3, Settings, Archive, ChevronDown, ChevronUp, Bell } from '@lucide/svelte'
  import type { Component } from 'svelte'
  import { navStore } from '../../lib/navigation.svelte.js'
  import { taskStore } from '../../stores/tasks.svelte.js'
  import { agentStore } from '../../stores/agents.svelte.js'
  import { notificationStore } from '../../stores/notifications.svelte.js'
  import { focusModeStore } from '../../lib/focus-mode.svelte.js'
  import { activeTaskNeedsUserAttention } from '../../lib/task-attention.js'

  // Focus mode collapses the rail to the core destinations; "More" expands the
  // full nav so advanced views stay reachable.
  let moreExpanded = $state(false)

  const interactiveAgentCount = $derived(
    agentStore.list.filter(a => a.mode === 'interactive' && (a.state === 'running' || a.state === 'paused')).length
  )

  const runningAgentCount = $derived(
    agentStore.list.filter(a => a.state === 'running').length
  )

  const reviewCount = $derived(
    taskStore.tasksNeedingPlanApproval().length
  )

  // Global "needs you" signal — every active task awaiting the operator,
  // regardless of which board column/filter is currently visible. Same
  // attention semantics as the board toolbar counter and card accent.
  const needsYouCount = $derived(
    taskStore.list.filter(activeTaskNeedsUserAttention).length
  )

  const notificationCount = $derived(notificationStore.notifications.length)

  interface NavItem {
    kind: string[]
    label: string
    icon: Component<{ size?: number }>
    title?: string
    onclick: () => void
  }

  // The mobile bottom bar already picks the real primaries — promote them to a
  // flat top section, tuck everything else into a flat secondary section, and
  // drop the WORK/SESSIONS/BUILD/DATA group headers. Settings is pinned bottom.
  const primaryItems: NavItem[] = [
    { kind: ['task-list', 'task-detail'], label: 'Board', icon: ClipboardList, onclick: () => navStore.reset({ kind: 'task-list' }) },
    { kind: ['chats', 'chat-detail'], label: 'Chats', icon: MessageCircle, onclick: () => navStore.reset({ kind: 'chats' }) },
    { kind: ['agents', 'agent-detail'], label: 'Agents', icon: UserCircle, onclick: () => navStore.reset({ kind: 'agents' }) },
    { kind: ['reviews'], label: 'Reviews', icon: ClipboardCheck, onclick: () => navStore.reset({ kind: 'reviews' }) },
  ]

  const secondaryItems: NavItem[] = [
    { kind: ['notifications'], label: 'Inbox', icon: Bell, onclick: () => navStore.reset({ kind: 'notifications' }) },
    { kind: ['logbook'], label: 'Logbook', icon: Archive, onclick: () => navStore.reset({ kind: 'logbook' }) },
    { kind: ['project-list', 'project-detail'], label: 'Projects', icon: Folder, onclick: () => navStore.reset({ kind: 'project-list' }) },
    { kind: ['workflows', 'workflow-detail'], label: 'Workflows', icon: LayoutDashboard, onclick: () => navStore.reset({ kind: 'workflows' }) },
    { kind: ['github'], label: 'GitHub', icon: GitBranch, onclick: () => navStore.reset({ kind: 'github' }) },
    { kind: ['stats'], label: 'Stats', icon: BarChart3, onclick: () => navStore.reset({ kind: 'stats' }) },
    { kind: ['evaluation'], label: 'Evaluation', icon: ClipboardCheck, onclick: () => navStore.reset({ kind: 'evaluation' }) },
  ]

  const settingsItem: NavItem = {
    kind: ['settings'],
    label: 'Settings',
    icon: Settings,
    title: 'Settings (Cmd+,)',
    onclick: () => navStore.reset({ kind: 'settings' }),
  }

  // Minimal rail only when focus mode is on and the user hasn't expanded "More".
  const minimal = $derived(focusModeStore.enabled && !moreExpanded)

  // Collapse "More" whenever focus mode turns off, so re-enabling starts minimal.
  $effect(() => {
    if (!focusModeStore.enabled) moreExpanded = false
  })
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
      {#if item.label === 'Board' && needsYouCount > 0}
        <span
          class="absolute -right-1 -top-1 flex h-3 w-3 items-center justify-center rounded-full bg-error-500 text-[8px] font-bold text-white"
          title="{needsYouCount} task{needsYouCount === 1 ? '' : 's'} need you"
          aria-label="{needsYouCount} task{needsYouCount === 1 ? '' : 's'} need you"
        >{needsYouCount}</span>
      {:else if item.label === 'Chats' && interactiveAgentCount > 0}
        <span class="absolute -right-1 -top-1 flex h-3 w-3 items-center justify-center rounded-full bg-primary-500 text-[8px] font-bold text-white">{interactiveAgentCount}</span>
      {:else if item.label === 'Agents' && runningAgentCount > 0}
        <span class="absolute -right-1 -top-1 flex h-3 w-3 items-center justify-center rounded-full bg-success-500 text-[8px] font-bold text-white">{runningAgentCount}</span>
      {:else if item.label === 'Reviews' && reviewCount > 0}
        <span class="absolute -right-1 -top-1 flex h-3 w-3 items-center justify-center rounded-full bg-warning-500 text-[8px] font-bold text-white">{reviewCount}</span>
      {:else if item.label === 'Inbox' && notificationCount > 0}
        <span class="absolute -right-1 -top-1 flex h-3 w-3 items-center justify-center rounded-full bg-secondary-500 text-[8px] font-bold text-white">{notificationCount}</span>
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
    {#if minimal}
      {#each primaryItems as item}
        {@render navButton(item)}
      {/each}
      <button
        type="button"
        onclick={() => (moreExpanded = true)}
        title="More"
        class="flex flex-col items-center gap-0.5 rounded-md px-1 py-1.5 text-[10px] font-medium text-surface-600 transition-colors hover:bg-surface-200 dark:text-surface-400 dark:hover:bg-surface-700"
      >
        <ChevronDown size={18} />
        <span class="leading-tight">More</span>
      </button>
    {:else}
      {#each primaryItems as item}
        {@render navButton(item)}
      {/each}
      <div class="mx-2 mb-0.5 mt-1.5 border-t border-surface-200 dark:border-surface-700"></div>
      {#each secondaryItems as item}
        {@render navButton(item)}
      {/each}
      {#if focusModeStore.enabled}
        <button
          type="button"
          onclick={() => (moreExpanded = false)}
          title="Less"
          class="mt-1.5 flex flex-col items-center gap-0.5 rounded-md px-1 py-1.5 text-[10px] font-medium text-surface-600 transition-colors hover:bg-surface-200 dark:text-surface-400 dark:hover:bg-surface-700"
        >
          <ChevronUp size={18} />
          <span class="leading-tight">Less</span>
        </button>
      {/if}
    {/if}
  </div>
  <div class="shrink-0 border-t border-surface-200 px-1 py-1 dark:border-surface-700">
    {@render navButton(settingsItem)}
  </div>
</nav>
