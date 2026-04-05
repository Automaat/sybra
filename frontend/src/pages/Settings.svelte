<script lang="ts">
  import { GetSettings, UpdateSettings } from '../../wailsjs/go/main/App.js'

  interface LoggingSettings {
    level: string
    maxSizeMB: number
    maxFiles: number
  }

  interface AuditConfig {
    enabled: boolean
    retentionDays: number
  }

  interface AgentDefaults {
    model: string
    mode: string
    maxConcurrent: number
  }

  interface NotificationConfig {
    desktop: boolean
  }

  interface OrchestratorConfig {
    autoTriage: boolean
    autoPlan: boolean
  }

  interface AppSettings {
    agent: AgentDefaults
    notification: NotificationConfig
    orchestrator: OrchestratorConfig
    logging: LoggingSettings
    audit: AuditConfig
    directories: Record<string, string>
  }

  let settings = $state<AppSettings | null>(null)
  let original = $state<string>('')
  let saving = $state(false)
  let error = $state('')
  let successMsg = $state('')

  const dirty = $derived(settings !== null && JSON.stringify(settings) !== original)

  const dirOrder = ['tasks', 'skills', 'projects', 'clones', 'worktrees', 'logs', 'audit']

  $effect(() => {
    load()
  })

  async function load() {
    try {
      const s = await GetSettings() as AppSettings
      settings = s
      original = JSON.stringify(s)
    } catch (e) {
      error = String(e)
    }
  }

  async function save() {
    if (!settings) return
    saving = true
    error = ''
    successMsg = ''
    try {
      await UpdateSettings(settings)
      original = JSON.stringify(settings)
      successMsg = 'Settings saved'
      setTimeout(() => { successMsg = '' }, 3000)
    } catch (e) {
      error = String(e)
    } finally {
      saving = false
    }
  }

  function reset() {
    if (!original) return
    settings = JSON.parse(original)
  }
</script>

<div class="flex flex-col gap-6 p-6">
  <div class="flex items-center justify-between">
    <h1 class="text-2xl font-bold">Settings</h1>
    <div class="flex items-center gap-2">
      {#if successMsg}
        <span class="text-sm text-success-500">{successMsg}</span>
      {/if}
      {#if error}
        <span class="text-sm text-error-500">{error}</span>
      {/if}
      {#if dirty}
        <button
          type="button"
          class="rounded-lg bg-surface-200 px-3 py-1.5 text-sm font-medium hover:bg-surface-300 dark:bg-surface-700 dark:hover:bg-surface-600"
          onclick={reset}
        >
          Reset
        </button>
      {/if}
      <button
        type="button"
        class="rounded-lg bg-primary-500 px-3 py-1.5 text-sm font-medium text-white hover:bg-primary-600 disabled:opacity-50"
        onclick={save}
        disabled={!dirty || saving}
      >
        {saving ? 'Saving…' : 'Save'}
      </button>
    </div>
  </div>

  {#if settings}
    <!-- Agent Defaults -->
    <div class="rounded-lg border border-surface-300 bg-surface-50 p-5 dark:border-surface-600 dark:bg-surface-800">
      <h2 class="mb-4 text-sm font-semibold text-surface-500 uppercase tracking-wide">Agent Defaults</h2>
      <div class="grid grid-cols-1 gap-4 sm:grid-cols-3">
        <div class="flex flex-col gap-1">
          <label class="text-sm font-medium" for="agent-model">Default Model</label>
          <select
            id="agent-model"
            class="rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
            bind:value={settings.agent.model}
          >
            <option value="">— none —</option>
            <option value="opus">Opus</option>
            <option value="sonnet">Sonnet</option>
            <option value="haiku">Haiku</option>
          </select>
        </div>
        <div class="flex flex-col gap-1">
          <label class="text-sm font-medium" for="agent-mode">Default Mode</label>
          <select
            id="agent-mode"
            class="rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
            bind:value={settings.agent.mode}
          >
            <option value="">— none —</option>
            <option value="headless">Headless</option>
            <option value="interactive">Interactive</option>
          </select>
        </div>
        <div class="flex flex-col gap-1">
          <label class="text-sm font-medium" for="agent-concurrency">Max Concurrent</label>
          <input
            id="agent-concurrency"
            type="number"
            min="1"
            max="10"
            class="rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
            bind:value={settings.agent.maxConcurrent}
          />
          <span class="text-xs text-surface-400">1–10</span>
        </div>
      </div>
    </div>

    <!-- Notifications -->
    <div class="rounded-lg border border-surface-300 bg-surface-50 p-5 dark:border-surface-600 dark:bg-surface-800">
      <h2 class="mb-4 text-sm font-semibold text-surface-500 uppercase tracking-wide">Notifications</h2>
      <label class="flex cursor-pointer items-center gap-3">
        <input
          type="checkbox"
          class="h-4 w-4 cursor-pointer rounded border-surface-300"
          bind:checked={settings.notification.desktop}
        />
        <span class="text-sm">Desktop notifications (macOS)</span>
      </label>
    </div>

    <!-- Orchestrator -->
    <div class="rounded-lg border border-surface-300 bg-surface-50 p-5 dark:border-surface-600 dark:bg-surface-800">
      <h2 class="mb-4 text-sm font-semibold text-surface-500 uppercase tracking-wide">Orchestrator</h2>
      <div class="flex flex-col gap-3">
        <label class="flex cursor-pointer items-center gap-3">
          <input
            type="checkbox"
            class="h-4 w-4 cursor-pointer rounded border-surface-300"
            bind:checked={settings.orchestrator.autoTriage}
          />
          <div>
            <span class="text-sm font-medium">Auto-triage</span>
            <p class="text-xs text-surface-400">Automatically dispatch triage agents on task creation</p>
          </div>
        </label>
        <label class="flex cursor-pointer items-center gap-3">
          <input
            type="checkbox"
            class="h-4 w-4 cursor-pointer rounded border-surface-300"
            bind:checked={settings.orchestrator.autoPlan}
          />
          <div>
            <span class="text-sm font-medium">Auto-plan</span>
            <p class="text-xs text-surface-400">Automatically dispatch planning agents on complex tasks</p>
          </div>
        </label>
      </div>
    </div>

    <!-- Logging & Audit -->
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

    <!-- Directories (read-only) -->
    <div class="rounded-lg border border-surface-300 bg-surface-50 p-5 dark:border-surface-600 dark:bg-surface-800">
      <h2 class="mb-4 text-sm font-semibold text-surface-500 uppercase tracking-wide">Directories</h2>
      <div class="flex flex-col gap-2">
        {#each dirOrder as key (key)}
          {#if settings.directories[key]}
            <div class="flex items-center gap-3">
              <span class="w-20 shrink-0 text-xs font-medium text-surface-400 capitalize">{key}</span>
              <input
                type="text"
                value={settings.directories[key]}
                disabled
                class="flex-1 rounded-lg border border-surface-200 bg-surface-100 px-3 py-1.5 font-mono text-xs text-surface-500 dark:border-surface-700 dark:bg-surface-900 dark:text-surface-400"
              />
            </div>
          {/if}
        {/each}
      </div>
    </div>
  {:else if error}
    <p class="text-error-500">{error}</p>
  {:else}
    <p class="text-surface-400">Loading…</p>
  {/if}
</div>
