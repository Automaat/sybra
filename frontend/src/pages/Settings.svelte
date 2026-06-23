<script lang="ts">
  import { onMount } from 'svelte'
  import { GetSettings, UpdateSettings, GetVersion, GetCodexModels } from '$lib/api'
  import type { AppSettings } from '../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'
  import ProviderHealthPanel from '../components/settings/ProviderHealthPanel.svelte'
  import LoggingPanel from '../components/settings/LoggingPanel.svelte'
  import TodoistPanel from '../components/settings/TodoistPanel.svelte'
  import RenovatePanel from '../components/settings/RenovatePanel.svelte'
  import { focusModeStore } from '../lib/focus-mode.svelte.js'
  import { viewModeStore } from '../lib/view-mode.svelte.js'

  function setFocusMode(on: boolean) {
    focusModeStore.set(on)
    // Focus mode leads with the simpler list view.
    if (on) viewModeStore.set('list')
  }

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

  let settings = $state<AppSettings | null>(null)
  let original = $state<string>('')
  let saving = $state(false)
  let error = $state('')
  let successMsg = $state('')

  const dirty = $derived(settings !== null && JSON.stringify(settings) !== original)

  const dirOrder = ['tasks', 'skills', 'projects', 'clones', 'worktrees', 'logs', 'audit']

  let serverVersion = $state<string | null>(null)
  const clientVersion = String(import.meta.env.VITE_APP_VERSION || 'dev')

  type ModelOption = { value: string; label: string }
  const codexFallbackModels: ModelOption[] = [
    { value: '', label: 'Default (gpt-5.4)' },
    { value: 'gpt-5.4', label: 'GPT-5.4' },
    { value: 'gpt-5.4-mini', label: 'GPT-5.4 Mini' },
    { value: 'gpt-5.3-codex', label: 'GPT-5.3 Codex' },
  ]
  let codexDynamicModels = $state<ModelOption[]>([])

  $effect(() => {
    load()
  })

  onMount(() => {
    GetVersion().then(v => { serverVersion = v.server }).catch(() => { serverVersion = 'unavailable' })
    GetCodexModels().then(models => {
      if (models && models.length > 0) {
        codexDynamicModels = [
          { value: '', label: codexFallbackModels[0].label },
          ...models.map(m => ({ value: m.slug, label: m.display_name })),
        ]
      }
    }).catch(() => {})
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

  function syncOriginal() {
    if (settings) original = JSON.stringify(settings)
  }

  const modelOptions = $derived.by(() => {
    if (!settings) return []
    if (settings.agent.provider === 'codex') {
      return codexDynamicModels.length > 0 ? codexDynamicModels : codexFallbackModels
    }
    return [
      { value: '', label: 'Default (Sonnet)' },
      { value: 'opus', label: 'Opus' },
      { value: 'sonnet', label: 'Sonnet' },
      { value: 'haiku', label: 'Haiku' },
    ]
  })

  // Keep provider/model pair valid in-memory: when provider flips and the
  // current model is not in that provider's option list, reset to default.
  $effect(() => {
    if (!settings) return
    const allowed = modelOptions.map((o) => o.value)
    if (!allowed.includes(settings.agent.model)) {
      settings.agent.model = ''
    }
  })
</script>

<div class="flex flex-col gap-4 p-4 md:gap-6 md:p-6">
  <div class="flex items-center justify-end">
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
    <label class="mt-4 flex items-start gap-3">
      <input
        type="checkbox"
        class="mt-0.5 h-4 w-4 accent-primary-500"
        checked={focusModeStore.enabled}
        onchange={(e) => setFocusMode((e.target as HTMLInputElement).checked)}
      />
      <span class="flex flex-col">
        <span class="text-sm font-medium">Focus mode</span>
        <span class="text-xs text-surface-400">Cleaner, minimal surface — collapses the sidebar and leads with the list view. Advanced views stay reachable via “More”.</span>
      </span>
    </label>
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

    <ProviderHealthPanel {settings} onsettingschange={syncOriginal} />

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

    <LoggingPanel settings={settings} />

    <TodoistPanel settings={settings} />

    <RenovatePanel settings={settings} />

    <!-- Version (read-only) -->
    <div class="rounded-lg border border-surface-300 bg-surface-50 p-5 dark:border-surface-600 dark:bg-surface-800">
      <h2 class="mb-4 text-sm font-semibold text-surface-500 uppercase tracking-wide">Version</h2>
      <div class="flex flex-col gap-2">
        <div class="flex items-center gap-3">
          <span class="w-20 shrink-0 text-xs font-medium text-surface-400">Server</span>
          <span class="flex-1 font-mono text-xs text-surface-500 dark:text-surface-400">{serverVersion ?? '…'}</span>
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
