<script lang="ts">
  import { Tabs } from '@skeletonlabs/skeleton-svelte'
  import AgentList from './AgentList.svelte'
  import Orchestrator from './Orchestrator.svelte'
  import TmuxSessions from './TmuxSessions.svelte'

  interface Props {
    onselect: (id: string) => void
    initialTab?: string
  }

  const { onselect, initialTab = 'agents' }: Props = $props()

  let activeTab = $state('agents')

  $effect(() => {
    activeTab = initialTab
  })
</script>

<div class="flex h-full flex-col overflow-hidden">
  <Tabs value={activeTab} onValueChange={(details) => (activeTab = details.value ?? 'agents')}>
    <div class="border-b border-surface-300 px-6 pt-2 dark:border-surface-600">
      <Tabs.List>
        <Tabs.Trigger value="agents">Agents</Tabs.Trigger>
        <Tabs.Trigger value="orchestrator">Orchestrator</Tabs.Trigger>
        <Tabs.Trigger value="sessions">Sessions</Tabs.Trigger>
        <Tabs.Indicator />
      </Tabs.List>
    </div>
    <div class="flex-1 overflow-y-auto">
      <Tabs.Content value="agents">
        <AgentList {onselect} />
      </Tabs.Content>
      <Tabs.Content value="orchestrator">
        <Orchestrator />
      </Tabs.Content>
      <Tabs.Content value="sessions">
        <TmuxSessions />
      </Tabs.Content>
    </div>
  </Tabs>
</div>
