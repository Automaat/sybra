<script lang="ts">
  import { ListTasks } from '../wailsjs/go/main/App.js'

  let tasks = $state<any[]>([])
  let error = $state('')

  async function loadTasks() {
    try {
      tasks = await ListTasks() ?? []
    } catch (e) {
      error = String(e)
    }
  }

  $effect(() => {
    loadTasks()
  })
</script>

<div class="h-full flex flex-col items-center justify-center gap-4 p-8">
  <h1 class="h1">Synapse</h1>
  <p class="text-lg opacity-60">Agent orchestrator</p>
  {#if error}
    <p class="text-error-500">{error}</p>
  {/if}
  <p class="text-sm opacity-40">{tasks.length} tasks</p>
</div>
