<script lang="ts">
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { agentStore } from '../../stores/agents.svelte.js'
  import { connectionStore } from '../../stores/connection.svelte.js'
  import { needsPlanApproval } from '../../lib/statuses.js'

  interface Props {
    task: Task
  }

  const { task }: Props = $props()

  let prompt = $state('')
  let agentMode = $state('interactive')
  let includeTaskDescription = $state(false)
  let starting = $state(false)
  let error = $state('')
  let modeInit = false

  $effect(() => {
    if (!modeInit && task.agentMode) {
      agentMode = task.agentMode
      modeInit = true
    }
  })

  async function startAgent() {
    if (!prompt.trim()) return
    starting = true
    error = ''
    try {
      // The started agent is surfaced by LiveAgentPanel (pinned above the tabs),
      // which tracks the running agent off the store — so this form only kicks
      // it off and resets.
      await agentStore.start(task.id, agentMode, prompt.trim(), includeTaskDescription)
      prompt = ''
    } catch (e) {
      error = String(e)
    } finally {
      starting = false
    }
  }
</script>

{#if error}
  <p class="text-xs text-error-500">{error}</p>
{/if}

{#if !needsPlanApproval(task)}
  <!-- A task awaiting plan approval's job is to approve/reject the plan (Plan
       tab), not launch a new ad-hoc run, so the generic "new run" form is
       collapsed for it. Keys off the workflow step (via needsPlanApproval) so a
       manual status desync can't leak the form back in (issue #1642). A live
       running agent still shows via LiveAgentPanel above the tabs. -->
  <div class="flex flex-col gap-3">
    <div class="flex flex-col gap-0.5">
      <span class="text-sm font-medium text-surface-500">New agent run</span>
      <span class="text-xs text-surface-400">
        Starts a fresh agent with the prompt below — not a continuation of an
        existing session.
      </span>
    </div>
    <div class="flex flex-wrap items-center gap-4">
      <label class="flex items-center gap-2">
        <input
          type="checkbox"
          checked={agentMode === 'headless'}
          onchange={(e) => { agentMode = e.currentTarget.checked ? 'headless' : 'interactive' }}
          class="rounded border-surface-300 dark:border-surface-600"
        />
        <span class="text-sm">Headless</span>
      </label>
      <label class="flex items-center gap-2">
        <input
          type="checkbox"
          checked={includeTaskDescription}
          onchange={(e) => { includeTaskDescription = e.currentTarget.checked }}
          class="rounded border-surface-300 dark:border-surface-600"
        />
        <span class="text-sm">Include task description</span>
      </label>
    </div>
    <textarea
      class="w-full resize-y rounded-lg border border-surface-300 bg-surface-50 p-3 text-sm dark:border-surface-600 dark:bg-surface-800"
      rows="3"
      placeholder="Enter prompt for the agent..."
      bind:value={prompt}
    ></textarea>
    <div class="flex items-center gap-2">
      <button
        type="button"
        class="w-fit rounded-lg bg-primary-500 px-4 py-2 text-sm font-medium text-white hover:bg-primary-600 disabled:cursor-not-allowed disabled:opacity-50"
        onclick={startAgent}
        disabled={starting || !prompt.trim() || !connectionStore.online}
        title={!connectionStore.online ? 'Offline — agent cannot start until connection is restored' : undefined}
      >
        {starting ? 'Starting...' : 'Start agent'}
      </button>
      {#if !connectionStore.online}
        <span class="text-xs text-warning-600 dark:text-warning-400">Offline</span>
      {/if}
    </div>
  </div>
{/if}
