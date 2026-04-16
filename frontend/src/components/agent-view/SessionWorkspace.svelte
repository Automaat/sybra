<script lang="ts">
  import { Tabs } from '@skeletonlabs/skeleton-svelte'
  import type { agent } from '../../../wailsjs/go/models.js'
  import type { TimestampedStreamEvent } from '$lib/timeline.js'
  import type { PlanStep } from '$lib/plan-steps.js'
  import { tabForTool, type TabKey, type ToolUseSignal } from '$lib/workspace-tabs.js'
  import ShellTab from '../workspace/ShellTab.svelte'
  import EditorTab from '../workspace/EditorTab.svelte'
  import PlannerTab from '../workspace/PlannerTab.svelte'

  interface Props {
    agentId: string
    taskId: string
    streamOutputs: TimestampedStreamEvent[]
    convoEvents: agent.ConvoEvent[]
    planSteps: PlanStep[]
    latestToolUse: ToolUseSignal | undefined
  }

  const { agentId, taskId, streamOutputs, convoEvents, planSteps, latestToolUse }: Props = $props()

  function getStorageString(key: string, fallback: string): string {
    if (typeof localStorage === 'undefined') return fallback
    return localStorage.getItem(key) ?? fallback
  }

  function getStorageBool(key: string, fallback: boolean): boolean {
    if (typeof localStorage === 'undefined') return fallback
    const val = localStorage.getItem(key)
    return val === null ? fallback : val === 'true'
  }

  let activeTab = $state<TabKey>(getStorageString('sybra.workspace.activeTab', 'shell') as TabKey)
  let following = $state(getStorageBool('sybra.workspace.following', true))

  // seenTabs tracks which tabs have had activity — resets when agentId changes.
  let seenTabs = $state(new Set<TabKey>())

  $effect(() => {
    // Depend on agentId so the cleanup runs when it changes.
    void agentId
    return () => {
      seenTabs = new Set<TabKey>()
    }
  })

  $effect(() => {
    localStorage.setItem('sybra.workspace.activeTab', activeTab)
  })

  $effect(() => {
    localStorage.setItem('sybra.workspace.following', String(following))
  })

  // Derived: ID of the latest edit-type tool use, for EditorTab cache key.
  const latestEditToolUseId = $derived(
    latestToolUse && (latestToolUse.name === 'Edit' || latestToolUse.name === 'Write' || latestToolUse.name === 'MultiEdit')
      ? latestToolUse.id
      : '',
  )

  let lastSeenId = $state<string | undefined>(undefined)

  // Following logic: auto-switch tab when latestToolUse changes.
  $effect(() => {
    if (!latestToolUse) return
    if (latestToolUse.id === lastSeenId) return
    lastSeenId = latestToolUse.id
    const tab = tabForTool(latestToolUse.name)
    if (tab) {
      seenTabs = new Set([...seenTabs, tab])
      if (following) {
        activeTab = tab
      }
    }
  })

  function handleTabClick(tab: TabKey) {
    activeTab = tab
    following = false
  }

  const TAB_LABELS: Record<TabKey, string> = {
    shell: 'Shell',
    editor: 'Editor',
    planner: 'Planner',
  }
</script>

<div class="flex h-full min-h-0 flex-col overflow-hidden">
  <!-- Tab bar + Following toggle -->
  <div class="flex items-center border-b border-surface-300 dark:border-surface-700">
    <Tabs
      value={activeTab}
      onValueChange={(d) => { handleTabClick((d.value ?? 'shell') as TabKey) }}
    >
      <Tabs.List>
        {#each (['shell', 'editor', 'planner'] as TabKey[]) as tab}
          <Tabs.Trigger value={tab}>
            <span class="flex items-center gap-1">
              {TAB_LABELS[tab]}
              {#if seenTabs.has(tab)}
                <span class="h-1.5 w-1.5 rounded-full bg-primary-500"></span>
              {/if}
            </span>
          </Tabs.Trigger>
        {/each}
        <Tabs.Indicator />
      </Tabs.List>
    </Tabs>

    <!-- Following toggle -->
    <label class="ml-auto flex cursor-pointer items-center gap-1.5 pr-2 text-xs text-surface-500 select-none">
      <input
        type="checkbox"
        role="switch"
        bind:checked={following}
        class="h-3.5 w-7 cursor-pointer appearance-none rounded-full border border-surface-400 bg-surface-200 transition-colors
          checked:border-primary-500 checked:bg-primary-500 dark:bg-surface-700"
        aria-label="Follow agent activity"
      />
      Follow
    </label>
  </div>

  <!-- Tab content -->
  <div class="min-h-0 flex-1 overflow-y-auto">
    {#if activeTab === 'shell'}
      <ShellTab {streamOutputs} {convoEvents} />
    {:else if activeTab === 'editor'}
      <EditorTab {taskId} {latestEditToolUseId} />
    {:else if activeTab === 'planner'}
      <PlannerTab {planSteps} />
    {/if}
  </div>
</div>
