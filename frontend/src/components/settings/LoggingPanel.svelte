<script lang="ts">
  import type { AppSettings } from '../../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'

  interface Props {
    settings: AppSettings
  }

  const { settings }: Props = $props()
</script>

<div class="rounded-lg border border-surface-300 bg-surface-50 p-5 dark:border-surface-600 dark:bg-surface-800">
  <h2 class="mb-4 text-sm font-semibold text-surface-500 uppercase tracking-wide">Logging & Audit</h2>
  <div class="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
    <div class="flex flex-col gap-1">
      <label class="text-sm font-medium" for="log-level">Log Level</label>
      <select
        id="log-level"
        class="rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
        bind:value={settings.logging.level}
      >
        <option value="debug">Debug</option>
        <option value="info">Info</option>
        <option value="warn">Warn</option>
        <option value="error">Error</option>
      </select>
    </div>
    <div class="flex flex-col gap-1">
      <label class="text-sm font-medium" for="log-max-size">Max Log Size (MB)</label>
      <input
        id="log-max-size"
        type="number"
        min="1"
        max="500"
        class="rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
        bind:value={settings.logging.maxSizeMB}
      />
      <span class="text-xs text-surface-400">1–500 MB</span>
    </div>
    <div class="flex flex-col gap-1">
      <label class="text-sm font-medium" for="log-max-files">Max Log Files</label>
      <input
        id="log-max-files"
        type="number"
        min="1"
        max="50"
        class="rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
        bind:value={settings.logging.maxFiles}
      />
      <span class="text-xs text-surface-400">1–50 files</span>
    </div>
    <div class="flex flex-col gap-1">
      <label class="text-sm font-medium" for="audit-retention">Audit Retention (days)</label>
      <input
        id="audit-retention"
        type="number"
        min="1"
        max="365"
        class="rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
        bind:value={settings.audit.retentionDays}
      />
      <span class="text-xs text-surface-400">1–365 days</span>
    </div>
  </div>
  <div class="mt-4">
    <label class="flex cursor-pointer items-center gap-3">
      <input
        type="checkbox"
        class="h-4 w-4 cursor-pointer rounded border-surface-300"
        bind:checked={settings.audit.enabled}
      />
      <span class="text-sm">Enable audit logging</span>
    </label>
  </div>
</div>
