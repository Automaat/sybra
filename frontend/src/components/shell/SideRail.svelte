<script lang="ts">
  import { Navigation } from '@skeletonlabs/skeleton-svelte'
  import { navStore } from '../../lib/navigation.svelte.js'
  import { taskStore } from '../../stores/tasks.svelte.js'
  import { agentStore } from '../../stores/agents.svelte.js'

  const interactiveAgentCount = $derived(
    agentStore.list.filter(a => a.mode === 'interactive' && (a.state === 'running' || a.state === 'paused')).length
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
      <svg class="h-5 w-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 6a2 2 0 012-2h2a2 2 0 012 2v2a2 2 0 01-2 2H6a2 2 0 01-2-2V6zM14 6a2 2 0 012-2h2a2 2 0 012 2v2a2 2 0 01-2 2h-2a2 2 0 01-2-2V6zM4 16a2 2 0 012-2h2a2 2 0 012 2v2a2 2 0 01-2 2H6a2 2 0 01-2-2v-2zM14 16a2 2 0 012-2h2a2 2 0 012 2v2a2 2 0 01-2 2h-2a2 2 0 01-2-2v-2z" />
      </svg>
      <Navigation.TriggerText>Dashboard</Navigation.TriggerText>
    </Navigation.Trigger>
    <Navigation.Trigger
      onclick={() => navStore.reset({ kind: 'task-list' })}
      data-active={navStore.page.kind === 'task-list' || navStore.page.kind === 'task-detail' || undefined}
    >
      <svg class="h-5 w-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 5H7a2 2 0 00-2 2v12a2 2 0 002 2h10a2 2 0 002-2V7a2 2 0 00-2-2h-2M9 5a2 2 0 002 2h2a2 2 0 002-2M9 5a2 2 0 012-2h2a2 2 0 012 2" />
      </svg>
      <Navigation.TriggerText>Board</Navigation.TriggerText>
    </Navigation.Trigger>
    <Navigation.Trigger
      onclick={() => navStore.reset({ kind: 'project-list' })}
      data-active={navStore.page.kind === 'project-list' || navStore.page.kind === 'project-detail' || undefined}
    >
      <svg class="h-5 w-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 7v10a2 2 0 002 2h14a2 2 0 002-2V9a2 2 0 00-2-2h-6l-2-2H5a2 2 0 00-2 2z" />
      </svg>
      <Navigation.TriggerText>Projects</Navigation.TriggerText>
    </Navigation.Trigger>
    <Navigation.Trigger
      onclick={() => navStore.reset({ kind: 'chats' })}
      data-active={navStore.page.kind === 'chats' || navStore.page.kind === 'chat-detail' || undefined}
    >
      <div class="relative">
        <svg class="h-5 w-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M8 12h.01M12 12h.01M16 12h.01M21 12c0 4.418-4.03 8-9 8a9.863 9.863 0 01-4.255-.949L3 20l1.395-3.72C3.512 15.042 3 13.574 3 12c0-4.418 4.03-8 9-8s9 3.582 9 8z" />
        </svg>
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
      <svg class="h-5 w-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9.75 17L9 20l-1 1h8l-1-1-.75-3M3 13h18M5 17h14a2 2 0 002-2V5a2 2 0 00-2-2H5a2 2 0 00-2 2v10a2 2 0 002 2z" />
      </svg>
      <Navigation.TriggerText>Agents</Navigation.TriggerText>
    </Navigation.Trigger>
    <Navigation.Trigger
      onclick={() => navStore.reset({ kind: 'github' })}
      data-active={navStore.page.kind === 'github' || undefined}
    >
      <svg class="h-5 w-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 2C6.477 2 2 6.477 2 12c0 4.42 2.865 8.166 6.839 9.489.5.092.682-.217.682-.482 0-.237-.009-.866-.013-1.7-2.782.604-3.369-1.341-3.369-1.341-.454-1.155-1.11-1.462-1.11-1.462-.908-.62.069-.608.069-.608 1.003.07 1.531 1.03 1.531 1.03.892 1.529 2.341 1.087 2.91.831.092-.646.35-1.086.636-1.337-2.22-.253-4.555-1.11-4.555-4.943 0-1.091.39-1.984 1.029-2.683-.103-.253-.446-1.27.098-2.647 0 0 .84-.269 2.75 1.025A9.578 9.578 0 0112 6.836a9.59 9.59 0 012.504.337c1.909-1.294 2.747-1.025 2.747-1.025.546 1.377.203 2.394.1 2.647.64.699 1.028 1.592 1.028 2.683 0 3.842-2.339 4.687-4.566 4.935.359.309.678.919.678 1.852 0 1.336-.012 2.415-.012 2.743 0 .267.18.578.688.48C19.138 20.163 22 16.418 22 12c0-5.523-4.477-10-10-10z" />
      </svg>
      <Navigation.TriggerText>GitHub</Navigation.TriggerText>
    </Navigation.Trigger>
    <Navigation.Trigger
      onclick={() => navStore.reset({ kind: 'reviews' })}
      data-active={navStore.page.kind === 'reviews' || undefined}
    >
      <div class="relative">
        <svg class="h-5 w-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 5H7a2 2 0 00-2 2v12a2 2 0 002 2h10a2 2 0 002-2V7a2 2 0 00-2-2h-2M9 5a2 2 0 002 2h2a2 2 0 002-2M9 5a2 2 0 012-2h2a2 2 0 012 2m-6 9l2 2 4-4" />
        </svg>
        {#if reviewCount > 0}
          <span class="absolute -right-1 -top-1 flex h-3.5 w-3.5 items-center justify-center rounded-full bg-warning-500 text-[9px] font-bold text-white">{reviewCount}</span>
        {/if}
      </div>
      <Navigation.TriggerText>Reviews</Navigation.TriggerText>
    </Navigation.Trigger>
    <Navigation.Trigger
      onclick={() => navStore.reset({ kind: 'workflows' })}
      data-active={navStore.page.kind === 'workflows' || navStore.page.kind === 'workflow-detail' || undefined}
    >
      <svg class="h-5 w-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 5a1 1 0 011-1h14a1 1 0 011 1v2a1 1 0 01-1 1H5a1 1 0 01-1-1V5zM4 13a1 1 0 011-1h6a1 1 0 011 1v6a1 1 0 01-1 1H5a1 1 0 01-1-1v-6zM16 13a1 1 0 011-1h2a1 1 0 011 1v6a1 1 0 01-1 1h-2a1 1 0 01-1-1v-6z" />
      </svg>
      <Navigation.TriggerText>Workflows</Navigation.TriggerText>
    </Navigation.Trigger>
    <Navigation.Trigger
      onclick={() => navStore.reset({ kind: 'stats' })}
      data-active={navStore.page.kind === 'stats' || undefined}
    >
      <svg class="h-5 w-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 19v-6a2 2 0 00-2-2H5a2 2 0 00-2 2v6a2 2 0 002 2h2a2 2 0 002-2zm0 0V9a2 2 0 012-2h2a2 2 0 012 2v10m-6 0a2 2 0 002 2h2a2 2 0 002-2m0 0V5a2 2 0 012-2h2a2 2 0 012 2v14a2 2 0 01-2 2h-2a2 2 0 01-2-2z" />
      </svg>
      <Navigation.TriggerText>Stats</Navigation.TriggerText>
    </Navigation.Trigger>
    <Navigation.Trigger
      onclick={() => navStore.reset({ kind: 'settings' })}
      data-active={navStore.page.kind === 'settings' || undefined}
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
