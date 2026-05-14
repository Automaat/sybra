<script lang="ts">
  import type { AppSettings } from '../../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'

  interface Props {
    settings: AppSettings
  }

  const { settings }: Props = $props()
</script>

<div class="rounded-lg border border-surface-300 bg-surface-50 p-5 dark:border-surface-600 dark:bg-surface-800">
  <h2 class="mb-4 text-sm font-semibold text-surface-500 uppercase tracking-wide">Todoist</h2>
  <div class="flex flex-col gap-4">
    <label class="flex cursor-pointer items-center gap-3">
      <input
        type="checkbox"
        class="h-4 w-4 cursor-pointer rounded border-surface-300"
        bind:checked={settings.todoist.enabled}
      />
      <div>
        <span class="text-sm font-medium">Enable Todoist sync</span>
        <p class="text-xs text-surface-400">Pull tasks from a Todoist project and close them when done</p>
      </div>
    </label>
    {#if settings.todoist.enabled}
      <div class="grid grid-cols-1 gap-4 sm:grid-cols-3">
        <div class="flex flex-col gap-1">
          <label class="text-sm font-medium" for="todoist-token">API Token</label>
          <input
            id="todoist-token"
            type="password"
            placeholder="Your Todoist API token"
            class="rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
            bind:value={settings.todoist.apiToken}
          />
          <span class="text-xs text-surface-400">Settings → Integrations → API token</span>
        </div>
        <div class="flex flex-col gap-1">
          <label class="text-sm font-medium" for="todoist-project">Project ID</label>
          <input
            id="todoist-project"
            type="text"
            placeholder="Todoist project ID"
            class="rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
            bind:value={settings.todoist.projectId}
          />
          <span class="text-xs text-surface-400">ID from project URL</span>
        </div>
        <div class="flex flex-col gap-1">
          <label class="text-sm font-medium" for="todoist-poll">Poll Interval (seconds)</label>
          <input
            id="todoist-poll"
            type="number"
            min="30"
            max="3600"
            class="rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
            bind:value={settings.todoist.pollSeconds}
          />
          <span class="text-xs text-surface-400">30–3600 seconds</span>
        </div>
      </div>
    {/if}
  </div>
</div>
