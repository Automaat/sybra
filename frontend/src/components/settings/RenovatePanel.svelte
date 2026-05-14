<script lang="ts">
  import type { AppSettings } from '../../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'

  interface Props {
    settings: AppSettings
  }

  const { settings }: Props = $props()
</script>

<div class="rounded-lg border border-surface-300 bg-surface-50 p-5 dark:border-surface-600 dark:bg-surface-800">
  <h2 class="mb-4 text-sm font-semibold text-surface-500 uppercase tracking-wide">Renovate</h2>
  <div class="flex flex-col gap-4">
    <label class="flex cursor-pointer items-center gap-3">
      <input
        type="checkbox"
        class="h-4 w-4 cursor-pointer rounded border-surface-300"
        bind:checked={settings.renovate.enabled}
      />
      <div>
        <span class="text-sm font-medium">Enable Renovate PR tracking</span>
        <p class="text-xs text-surface-400">Show Renovate bot PRs for registered projects</p>
      </div>
    </label>
    {#if settings.renovate.enabled}
      <div class="flex flex-col gap-1 sm:max-w-sm">
        <label class="text-sm font-medium" for="renovate-author">PR Author</label>
        <input
          id="renovate-author"
          type="text"
          placeholder="app/renovate"
          class="rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
          bind:value={settings.renovate.author}
        />
        <span class="text-xs text-surface-400">GitHub author filter (default: app/renovate)</span>
      </div>
    {/if}
  </div>
</div>
