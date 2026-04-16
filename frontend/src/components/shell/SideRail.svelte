<script lang="ts">
  import { LayoutGrid, ClipboardList, Folder, MessageCircle, UserCircle, GitBranch, ClipboardCheck, LayoutDashboard, BarChart3, Settings, Archive } from '@lucide/svelte'
  import { Navigation } from '@skeletonlabs/skeleton-svelte'
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
</script>

<Navigation layout="rail">
  <Navigation.Header>
    <span class="p-2 text-lg font-bold">S</span>
  </Navigation.Header>
  <Navigation.Content>
    <Navigation.Trigger
      onclick={() => navStore.reset({ kind: 'dashboard' })}
      data-active={navStore.page.kind === 'dashboard' || undefined}
    >
      <LayoutGrid size={20} />
      <Navigation.TriggerText>Dashboard</Navigation.TriggerText>
    </Navigation.Trigger>
    <Navigation.Trigger
      onclick={() => navStore.reset({ kind: 'task-list' })}
      data-active={navStore.page.kind === 'task-list' || navStore.page.kind === 'task-detail' || undefined}
    >
      <ClipboardList size={20} />
      <Navigation.TriggerText>Board</Navigation.TriggerText>
    </Navigation.Trigger>
    <Navigation.Trigger
      onclick={() => navStore.reset({ kind: 'project-list' })}
      data-active={navStore.page.kind === 'project-list' || navStore.page.kind === 'project-detail' || undefined}
    >
      <Folder size={20} />
      <Navigation.TriggerText>Projects</Navigation.TriggerText>
    </Navigation.Trigger>
    <Navigation.Trigger
      onclick={() => navStore.reset({ kind: 'chats' })}
      data-active={navStore.page.kind === 'chats' || navStore.page.kind === 'chat-detail' || undefined}
    >
      <div class="relative">
        <MessageCircle size={20} />
        {#if interactiveAgentCount > 0}
          <span class="absolute -right-1 -top-1 flex h-3.5 w-3.5 items-center justify-center rounded-full bg-primary-500 text-[9px] font-bold text-white">{interactiveAgentCount}</span>
        {/if}
      </div>
      <Navigation.TriggerText>Chats</Navigation.TriggerText>
    </Navigation.Trigger>
    <Navigation.Trigger
      onclick={() => navStore.reset({ kind: 'agents' })}
      data-active={navStore.page.kind === 'agents' || navStore.page.kind === 'agent-detail' || undefined}
    >
      <div class="relative">
        <UserCircle size={20} />
        {#if runningAgentCount > 0}
          <span class="absolute -right-1 -top-1 flex h-3.5 w-3.5 items-center justify-center rounded-full bg-success-500 text-[9px] font-bold text-white">{runningAgentCount}</span>
        {/if}
      </div>
      <Navigation.TriggerText>Agents</Navigation.TriggerText>
    </Navigation.Trigger>
    <Navigation.Trigger
      onclick={() => navStore.reset({ kind: 'github' })}
      data-active={navStore.page.kind === 'github' || undefined}
    >
      <GitBranch size={20} />
      <Navigation.TriggerText>GitHub</Navigation.TriggerText>
    </Navigation.Trigger>
    <Navigation.Trigger
      onclick={() => navStore.reset({ kind: 'reviews' })}
      data-active={navStore.page.kind === 'reviews' || undefined}
    >
      <div class="relative">
        <ClipboardCheck size={20} />
        {#if reviewCount > 0}
          <span class="absolute -right-1 -top-1 flex h-3.5 w-3.5 items-center justify-center rounded-full bg-warning-500 text-[9px] font-bold text-white">{reviewCount}</span>
        {/if}
      </div>
      <Navigation.TriggerText>Reviews</Navigation.TriggerText>
    </Navigation.Trigger>
    <Navigation.Trigger
      onclick={() => navStore.reset({ kind: 'logbook' })}
      data-active={navStore.page.kind === 'logbook' || undefined}
    >
      <Archive size={20} />
      <Navigation.TriggerText>Logbook</Navigation.TriggerText>
    </Navigation.Trigger>
    <Navigation.Trigger
      onclick={() => navStore.reset({ kind: 'workflows' })}
      data-active={navStore.page.kind === 'workflows' || navStore.page.kind === 'workflow-detail' || undefined}
    >
      <LayoutDashboard size={20} />
      <Navigation.TriggerText>Workflows</Navigation.TriggerText>
    </Navigation.Trigger>
    <Navigation.Trigger
      onclick={() => navStore.reset({ kind: 'stats' })}
      data-active={navStore.page.kind === 'stats' || undefined}
    >
      <BarChart3 size={20} />
      <Navigation.TriggerText>Stats</Navigation.TriggerText>
    </Navigation.Trigger>
    <Navigation.Trigger
      onclick={() => navStore.reset({ kind: 'settings' })}
      data-active={navStore.page.kind === 'settings' || undefined}
      title="Settings (Cmd+,)"
    >
      <Settings size={20} />
      <Navigation.TriggerText>Settings</Navigation.TriggerText>
    </Navigation.Trigger>
  </Navigation.Content>
</Navigation>
