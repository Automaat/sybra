<script lang="ts">
  import { GetSettings, UpdateSettings, GetVersion } from '$lib/api'
  import type { synapse } from '../../wailsjs/go/models.js'
  import {
    GetProviderHealth,
    ProviderHealthEnabled,
    SetProviderAutoFailover,
    SetProviderEnabled,
  } from '../../wailsjs/go/synapse/IntegrationService'
  import { EventsOn } from '$lib/api'
  import * as ev from '../lib/events.js'

  type ColorScheme = 'system' | 'light' | 'dark'

  let colorScheme = $state<ColorScheme>(
    (localStorage.getItem('colorScheme') ?? 'system') as ColorScheme
  )

  function applyColorScheme(scheme: ColorScheme) {
    const prefersDark = window.matchMedia('(prefers-color-scheme: dark)').matches
    const isDark = scheme === 'dark' || (scheme === 'system' && prefersDark)
    document.documentElement.classList.toggle('dark', isDark)
    localStorage.setItem('colorScheme', scheme)
  }

  $effect(() => {
    applyColorScheme(colorScheme)
  })

  type AppSettings = synapse.AppSettings

  let settings = $state<AppSettings | null>(null)
  let original = $state<string>('')
  let saving = $state(false)
  let error = $state('')
  let successMsg = $state('')

  const dirty = $derived(settings !== null && JSON.stringify(settings) !== original)

  const dirOrder = ['tasks', 'skills', 'projects', 'clones', 'worktrees', 'logs', 'audit']

  let serverVersion = $state<string>('')
  const clientVersion = String(import.meta.env.VITE_APP_VERSION || 'dev')

  $effect(() => {
    load()
    GetVersion().then(v => { serverVersion = v.server }).catch(() => {})
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

  type ProviderHealthEntry = {
    provider: string
    healthy: boolean
    reason: string
    detail?: string
    lastCheck?: string
    ratelimitedUntil?: string
  }
  let providerHealthEnabled = $state(false)
  let providerHealthMap = $state<Record<string, ProviderHealthEntry>>({})
  let providerError = $state('')

  async function loadProviderHealth() {
    try {
      providerHealthEnabled = await ProviderHealthEnabled()
      if (!providerHealthEnabled) return
      const list = (await GetProviderHealth()) ?? []
      const next: Record<string, ProviderHealthEntry> = {}
      for (const p of list) next[p.provider] = p as ProviderHealthEntry
      providerHealthMap = next
    } catch (e) {
      providerError = String(e)
    }
  }

  $effect(() => {
    loadProviderHealth()
    const unsub = EventsOn(ev.ProviderHealth, (p: ProviderHealthEntry) => {
      if (!p?.provider) return
      providerHealthMap = { ...providerHealthMap, [p.provider]: p }
    })
    return () => unsub()
  })

  async function onAutoFailoverChange(e: Event) {
    if (!settings) return
    const value = (e.target as HTMLInputElement).checked
    try {
      await SetProviderAutoFailover(value)
      settings.providers.autoFailover = value
      original = JSON.stringify(settings)
    } catch (err) {
      providerError = String(err)
    }
  }

  async function onProviderEnabledChange(name: 'claude' | 'codex', e: Event) {
    if (!settings) return
    const value = (e.target as HTMLInputElement).checked
    try {
      await SetProviderEnabled(name, value)
      settings.providers[name].enabled = value
      original = JSON.stringify(settings)
      await loadProviderHealth()
    } catch (err) {
      providerError = String(err)
    }
  }

  function healthBadgeClass(p: ProviderHealthEntry): string {
    if (p.healthy) return 'bg-success-500/20 text-success-600 dark:text-success-300'
    if (p.reason === 'disabled') return 'bg-surface-300 text-surface-600 dark:bg-surface-600 dark:text-surface-300'
    return 'bg-error-500/20 text-error-600 dark:text-error-300'
  }

  const modelOptions = $derived.by(() => {
    if (!settings) return []
    if (settings.agent.provider === 'codex') {
      return [
        { value: '', label: 'Default (gpt-5.4)' },
        { value: 'gpt-5.4', label: 'GPT-5.4' },
        { value: 'gpt-5.4-mini', label: 'GPT-5.4 Mini' },
        { value: 'gpt-5.3-codex', label: 'GPT-5.3 Codex' },
      ]
    }
    return [
      { value: '', label: 'Default (Sonnet)' },
      { value: 'opus', label: 'Opus' },
      { value: 'sonnet', label: 'Sonnet' },
      { value: 'haiku', label: 'Haiku' },
    ]
  })
</script>

<div class="flex flex-col gap-4 p-4 md:gap-6 md:p-6">
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

  <!-- Appearance (localStorage-backed, no save required) -->
  <div class="rounded-lg border border-surface-300 bg-surface-50 p-5 dark:border-surface-600 dark:bg-surface-800">
    <h2 class="mb-4 text-sm font-semibold text-surface-500 uppercase tracking-wide">Appearance</h2>
    <div class="flex flex-col gap-1 sm:max-w-xs">
      <label class="text-sm font-medium" for="color-scheme">Color Scheme</label>
      <select
        id="color-scheme"
        class="rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
        bind:value={colorScheme}
      >
        <option value="system">System</option>
        <option value="light">Light</option>
        <option value="dark">Dark</option>
      </select>
      <span class="text-xs text-surface-400">Applied immediately, no save needed</span>
    </div>
  </div>

  {#if settings}
    <!-- Agent Defaults -->
    <div class="rounded-lg border border-surface-300 bg-surface-50 p-5 dark:border-surface-600 dark:bg-surface-800">
      <h2 class="mb-4 text-sm font-semibold text-surface-500 uppercase tracking-wide">Agent Defaults</h2>
      <div class="grid grid-cols-1 gap-4 sm:grid-cols-4">
        <div class="flex flex-col gap-1">
          <label class="text-sm font-medium" for="agent-provider">Agent Type</label>
          <select
            id="agent-provider"
            class="rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
            bind:value={settings.agent.provider}
          >
            <option value="claude">Claude</option>
            <option value="codex">Codex</option>
          </select>
        </div>
        <div class="flex flex-col gap-1">
          <label class="text-sm font-medium" for="agent-model">Default Model</label>
          <select
            id="agent-model"
            class="rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
            bind:value={settings.agent.model}
          >
            {#each modelOptions as option}
              <option value={option.value}>{option.label}</option>
            {/each}
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

    <!-- Providers -->
    {#if providerHealthEnabled}
      <div class="rounded-lg border border-surface-300 bg-surface-50 p-5 dark:border-surface-600 dark:bg-surface-800">
        <h2 class="mb-4 text-sm font-semibold text-surface-500 uppercase tracking-wide">Providers</h2>
        {#if providerError}
          <div class="mb-3 text-xs text-error-500">{providerError}</div>
        {/if}
        <div class="flex flex-col gap-3">
          {#each ['claude', 'codex'] as name (name)}
            {@const p = providerHealthMap[name]}
            <div class="flex items-center justify-between gap-3 rounded border border-surface-200 bg-white px-3 py-2 dark:border-surface-700 dark:bg-surface-900">
              <div class="flex flex-col">
                <div class="flex items-center gap-2">
                  <span class="font-medium capitalize">{name}</span>
                  {#if p}
                    <span class="rounded px-1.5 py-0.5 text-xs {healthBadgeClass(p)}">
                      {p.healthy ? 'healthy' : p.reason}
                    </span>
                  {/if}
                </div>
                {#if p?.detail}
                  <span class="text-xs text-surface-400">{p.detail}</span>
                {/if}
                {#if p?.lastCheck}
                  <span class="text-xs text-surface-400">last check: {new Date(p.lastCheck).toLocaleTimeString()}</span>
                {/if}
              </div>
              <label class="flex cursor-pointer items-center gap-2 text-sm">
                <input
                  type="checkbox"
                  class="h-4 w-4 cursor-pointer rounded border-surface-300"
                  checked={name === 'claude' ? settings.providers.claude.enabled : settings.providers.codex.enabled}
                  onchange={(e) => onProviderEnabledChange(name as 'claude' | 'codex', e)}
                />
                <span>Enabled</span>
              </label>
            </div>
          {/each}
          <label class="flex cursor-pointer items-center gap-3 pt-2">
            <input
              type="checkbox"
              class="h-4 w-4 cursor-pointer rounded border-surface-300"
              checked={settings.providers.autoFailover}
              onchange={onAutoFailoverChange}
            />
            <span class="text-sm">Auto-failover between providers when one is unhealthy</span>
          </label>
          <span class="text-xs text-surface-400">
            Health check interval: {settings.providers.healthCheck.intervalSeconds}s. Edit config.yaml to change.
          </span>
        </div>
      </div>
    {/if}

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

    <!-- Todoist -->
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

    <!-- Renovate -->
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

    <!-- Version (read-only) -->
    <div class="rounded-lg border border-surface-300 bg-surface-50 p-5 dark:border-surface-600 dark:bg-surface-800">
      <h2 class="mb-4 text-sm font-semibold text-surface-500 uppercase tracking-wide">Version</h2>
      <div class="flex flex-col gap-2">
        <div class="flex items-center gap-3">
          <span class="w-20 shrink-0 text-xs font-medium text-surface-400">Server</span>
          <span class="flex-1 font-mono text-xs text-surface-500 dark:text-surface-400">{serverVersion || '…'}</span>
        </div>
        <div class="flex items-center gap-3">
          <span class="w-20 shrink-0 text-xs font-medium text-surface-400">Client</span>
          <span class="flex-1 font-mono text-xs text-surface-500 dark:text-surface-400">{clientVersion}</span>
        </div>
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
