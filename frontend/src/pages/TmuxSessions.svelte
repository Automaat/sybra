<script lang="ts">
  import { ListTmuxSessions, KillTmuxSession, AttachTmuxSession } from '../../wailsjs/go/main/AgentService.js'
  import type { tmux } from '../../wailsjs/go/models.js'

  let sessions = $state<tmux.SessionInfo[]>([])
  let loading = $state(true)
  let error = $state('')

  async function load() {
    try {
      loading = true
      error = ''
      const result = await ListTmuxSessions()
      sessions = result ?? []
    } catch (e) {
      error = String(e)
      sessions = []
    } finally {
      loading = false
    }
  }

  async function kill(name: string) {
    try {
      await KillTmuxSession(name)
      await load()
    } catch (e) {
      error = String(e)
    }
  }

  async function attach(name: string) {
    try {
      await AttachTmuxSession(name)
    } catch (e) {
      error = String(e)
    }
  }

  $effect(() => {
    load()
    const interval = setInterval(load, 5000)
    return () => clearInterval(interval)
  })
</script>

<div class="flex flex-col gap-4 p-6">
  <div class="flex items-center justify-between">
    <p class="text-sm opacity-60">{sessions.length} session{sessions.length !== 1 ? 's' : ''}</p>
    <button
      type="button"
      class="rounded-lg bg-surface-200 px-3 py-1.5 text-sm font-medium hover:bg-surface-300 dark:bg-surface-700 dark:hover:bg-surface-600"
      onclick={load}
    >
      Refresh
    </button>
  </div>

  {#if loading && sessions.length === 0}
    <p class="text-center text-sm opacity-60">Loading sessions...</p>
  {:else if error}
    <p class="text-center text-sm text-error-500">{error}</p>
  {:else if sessions.length === 0}
    <div class="flex flex-col items-center gap-2 py-16 opacity-50">
      <p class="text-lg">No tmux sessions</p>
      <p class="text-sm">Start an interactive agent to create a tmux session</p>
    </div>
  {:else}
    <div class="flex flex-col gap-2">
      {#each sessions as session (session.name)}
        <div class="flex items-center justify-between rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
          <div class="flex flex-col gap-1">
            <span class="font-mono text-sm font-semibold">{session.name}</span>
            {#if session.created}
              <span class="text-xs opacity-50">Created: {session.created}</span>
            {/if}
          </div>
          <div class="flex gap-2">
            <button
              type="button"
              class="rounded-lg bg-primary-500 px-3 py-1.5 text-sm font-medium text-white hover:bg-primary-600"
              onclick={() => attach(session.name)}
            >
              Attach
            </button>
            <button
              type="button"
              class="rounded-lg bg-error-500 px-3 py-1.5 text-sm font-medium text-white hover:bg-error-600"
              onclick={() => kill(session.name)}
            >
              Kill
            </button>
          </div>
        </div>
      {/each}
    </div>
  {/if}
</div>
